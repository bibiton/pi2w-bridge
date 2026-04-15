import asyncio
import json
from fastapi import FastAPI, WebSocket
from fastapi.staticfiles import StaticFiles
import subprocess
import shlex
import rclpy
from rclpy.node import Node
from std_msgs.msg import String, Bool
from fastapi.responses import JSONResponse
from fastapi.middleware.cors import CORSMiddleware
from geometry_msgs.msg import PoseStamped, Twist, PoseWithCovarianceStamped
from fastapi.responses import JSONResponse, FileResponse
import yaml
from PIL import Image
import numpy as np
from nav_msgs.msg import OccupancyGrid, Path
from sensor_msgs.msg import LaserScan
from nav_msgs.msg import OccupancyGrid as CostmapMsg
import math
import time
from tf2_ros import Buffer, TransformListener, LookupException, ConnectivityException, ExtrapolationException
from rclpy.duration import Duration
from rclpy.qos import qos_profile_sensor_data
import tf2_geometry_msgs

app = FastAPI() 
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],  # 或指定你的前端網域
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

ros_node = None
# ws_clients = []
loop = asyncio.get_event_loop()




class StatusSubscriber(Node):
    def __init__(self, loop):
        super().__init__('web_interface_node')
        self.loop = loop
        self.ws_clients = []
        self.tracked_pose_enabled = False   # 記錄狀態
        self.tracked_pose_sub = None        # 暫存訂閱 handler
        
        # ROS 處理間隔參數（可動態調整）
        self.spin_timeout = 0.01  # 預設 10ms sleep 間隔
        self.spin_running = True  # 控制 spin loop

        # TF2 Buffer and Listener for coordinate transformations
        self.tf_buffer = Buffer()
        self.tf_listener = TransformListener(self.tf_buffer, self)
        
        # 儲存最新的機器人位置（用於 LiDAR 轉換）
        self.robot_x = 0.0
        self.robot_y = 0.0
        self.robot_yaw = 0.0

        # LiDAR 訂閱狀態
        self.front_lidar_enabled = False
        self.front_lidar_sub = None
        self.rear_lidar_enabled = False
        self.rear_lidar_sub = None
        
        # Frame skipping for performance
        self.front_frame_counter = 0
        self.front_frame_skip = 1  # 每 N 幀處理一次
        self.front_ray_step = 1    # 每 N 個點取一個
        self.rear_frame_counter = 0
        self.rear_frame_skip = 1
        self.rear_ray_step = 1
        
        # Path 訂閱狀態
        self.path_enabled = False
        self.path_sub = None
        
        # Costmap 訂閱狀態
        self.costmap_enabled = False
        self.costmap_sub = None

        # self.subscription = self.create_subscription(
        #     String,
        #     '/route_ctrl_event',
        #     self.listener_callback,
        #     10)

        self.subs = {
            "/route_ctrl_event": self.create_subscription(
                String, "/route_ctrl_event",
                lambda msg: self.listener_callback("/route_ctrl_event", msg), 10),
            # "/robot_status": self.create_subscription(
            #     String, "/robot_status",
            #     lambda msg: self.listener_callback("/robot_status", msg), 10),
            # 可以再加第三、第四個 topic...
        }

        # where to save the latest live map image
        self.live_map_path = "/tmp/live_map.png"
        self.last_map_emit = 0.0      # for 1 Hz throttle
        self.live_version = 0         # (optional) for versioned cache-busting

        # NEW: subscribe to /map (OccupancyGrid)
        self.map_sub = self.create_subscription(
            OccupancyGrid, "/map", self.map_callback, 10)


        self.publisher_route_ctrl = self.create_publisher(String, '/route_ctrl_event', 10)
        self.publisher_ui = self.create_publisher(String, '/ui_event', 10)
        self.publisher_cmd_vel = self.create_publisher(Twist, '/cmd_vel', 10)
        self.publisher_stop_motor = self.create_publisher(Bool, '/stop_motor', 10)
        self.publisher_initial_pose = self.create_publisher(PoseWithCovarianceStamped, '/vslam/pose', 10)

        # Nav2 goal publisher
        self.publisher_goal_pose = self.create_publisher(PoseStamped, '/goal_pose', 10)


    def map_callback(self, msg: OccupancyGrid):
        """
        Convert OccupancyGrid to a grayscale PNG and notify clients.
        - occ:   0 (free)    -> 255 (white)
        - occ: 100 (occupied)->   0 (black)
        - occ:  -1 (unknown) -> 205 (mid-gray)
        """

        # --- throttle: emit at most 1 Hz ---
        now = time.time()
        if now - self.last_map_emit < 1.0:
            return
        self.last_map_emit = now


        info = msg.info
        w, h = info.width, info.height
        data = np.array(msg.data, dtype=np.int16).reshape(h, w)

        # map to grayscale
        img = np.zeros((h, w), dtype=np.uint8)
        # free
        img[data == 0] = 255
        # occupied
        img[data == 100] = 0
        # unknown
        img[data == -1] = 205
        # linear mapping for 1..99 if present
        mask_lin = (data > 0) & (data < 100)
        img[mask_lin] = (255 - (data[mask_lin] * 255 // 100)).astype(np.uint8)

        # Flip vertically so (0,0) bottom-left in ROS becomes bottom in image display
        img = np.flipud(img)

        # save as PNG
        Image.fromarray(img, mode='L').save(self.live_map_path)


        self.live_version += 1  
        # notify all ws clients with live map meta (same shape as /map_meta endpoint)
        meta = {
            "width": int(w),
            "height": int(h),
            "resolution": float(info.resolution),
            "origin": [float(info.origin.position.x), float(info.origin.position.y)],
            "image": f"/map_live_image?v={self.live_version}"
        }
        payload = json.dumps({ "topic": "/map_meta", "data": meta })
        for client in self.ws_clients:
            asyncio.run_coroutine_threadsafe(client.send_text(payload), self.loop)


    def listener_callback(self, topic, msg):
        data = msg.data
        print(f"[ROS][{topic}] 接收到: {data}")

        # 嘗試解析成 dict，如果有 "event" key 就略過
        try:
            json_obj = json.loads(data)
            # 如果是 dict 且含有 event key，就 return 不送出
            if isinstance(json_obj, dict) and "event" in json_obj:
                print(f"[ROS][{topic}] 發現 event key，已過濾不發送")
                return
        except Exception:
            # 無法解析成 json 就照常處理
            pass

        for client in self.ws_clients:
            # 統一用 JSON 格式發送
            asyncio.run_coroutine_threadsafe(
                client.send_text(json.dumps({
                    "topic": topic,
                    "data": data
                })), self.loop)




    def send_cmd_vel(self, linear_x, angular_z):
        twist = Twist()
        twist.linear.x = float(linear_x)
        twist.angular.z = float(angular_z)
        self.publisher_cmd_vel.publish(twist)
        self.get_logger().info(f'[cmd_vel] 發送: linear_x={linear_x}, angular_z={angular_z}')



    def send_stop_motor(self, value: bool):
        msg = Bool()
        msg.data = value
        self.publisher_stop_motor.publish(msg)
        self.get_logger().info(f"[stop_motor] 發送: {value}")

    def send_initial_pose(self, x, y, yaw):
        try:
            msg = PoseWithCovarianceStamped()
            msg.header.stamp = self.get_clock().now().to_msg()
            msg.header.frame_id = "map"
            msg.pose.pose.position.x = float(x)
            msg.pose.pose.position.y = float(y)
            msg.pose.pose.position.z = 0.0
            
            # 將 yaw 轉換為四元數
            cy = math.cos(yaw * 0.5)
            sy = math.sin(yaw * 0.5)
            msg.pose.pose.orientation.w = cy
            msg.pose.pose.orientation.x = 0.0
            msg.pose.pose.orientation.y = 0.0
            msg.pose.pose.orientation.z = sy
            
            # 設置協方差矩陣（6x6 = 36 個元素）
            # 對角線元素表示 x, y, z, roll, pitch, yaw 的不確定性
            msg.pose.covariance = [0.0] * 36
            msg.pose.covariance[0] = 0.25   # x 方差
            msg.pose.covariance[7] = 0.25   # y 方差
            msg.pose.covariance[35] = 0.06853891909122467  # yaw 方差 (約 15 度)
            
            self.get_logger().info(f"[initial_pose] 準備發送到 /vslam/pose: x={x}, y={y}, yaw={yaw}")
            self.publisher_initial_pose.publish(msg)
            self.get_logger().info(f"[initial_pose] 已發送")
        except Exception as e:
            self.get_logger().error(f"[initial_pose] 發送失敗: {e}")

    def send_goal_pose(self, x, y, yaw):
        """發送 Nav2 導航目標"""
        try:
            msg = PoseStamped()
            msg.header.stamp = self.get_clock().now().to_msg()
            msg.header.frame_id = "map"
            msg.pose.position.x = float(x)
            msg.pose.position.y = float(y)
            msg.pose.position.z = 0.0

            # yaw → quaternion
            cy = math.cos(yaw * 0.5)
            sy = math.sin(yaw * 0.5)
            msg.pose.orientation.w = cy
            msg.pose.orientation.x = 0.0
            msg.pose.orientation.y = 0.0
            msg.pose.orientation.z = sy

            self.publisher_goal_pose.publish(msg)
            self.get_logger().info(f"[goal_pose] 導航到: x={x}, y={y}, yaw={yaw}")
        except Exception as e:
            self.get_logger().error(f"[goal_pose] 發送失敗: {e}")

    def cancel_navigation(self):
        """取消 Nav2 導航"""
        try:
            subprocess.Popen(['ros2', 'topic', 'pub', '--once',
                '/navigate_to_pose/_action/cancel_goal',
                'action_msgs/msg/CancelGoal', '{}'])
            self.get_logger().info("[cancel_nav] 已發送取消導航指令")
        except Exception as e:
            self.get_logger().error(f"[cancel_nav] 取消失敗: {e}")






    def enable_tracked_pose(self):
        if not self.tracked_pose_enabled:
            self.tracked_pose_enabled = True
            self.tracked_pose_sub = self.create_subscription(
                PoseStamped, "/tracked_pose", self.tracked_pose_callback, 10)
            self.get_logger().info('[tracked_pose] 訂閱已啟用')

    def disable_tracked_pose(self):
        if self.tracked_pose_enabled and self.tracked_pose_sub:
            self.destroy_subscription(self.tracked_pose_sub)
            self.tracked_pose_sub = None
            self.tracked_pose_enabled = False
            self.get_logger().info('[tracked_pose] 訂閱已停用')

    def tracked_pose_callback(self, msg):
        # 更新機器人位置（用於 LiDAR 轉換）
        self.robot_x = msg.pose.position.x
        self.robot_y = msg.pose.position.y
        
        # 從四元數計算 yaw
        q = msg.pose.orientation
        siny_cosp = 2.0 * (q.w * q.z + q.x * q.y)
        cosy_cosp = 1.0 - 2.0 * (q.y * q.y + q.z * q.z)
        self.robot_yaw = math.atan2(siny_cosp, cosy_cosp)
        
        # 根據需求轉 dict
        pose_dict = {
            "header": {
                "stamp": {
                    "sec": msg.header.stamp.sec,
                    "nanosec": msg.header.stamp.nanosec
                },
                "frame_id": msg.header.frame_id
            },
            "pose": {
                "position": {
                    "x": msg.pose.position.x,
                    "y": msg.pose.position.y,
                    "z": msg.pose.position.z
                },
                "orientation": {
                    "x": msg.pose.orientation.x,
                    "y": msg.pose.orientation.y,
                    "z": msg.pose.orientation.z,
                    "w": msg.pose.orientation.w
                }
            }
        }
        for client in self.ws_clients:
            asyncio.run_coroutine_threadsafe(
                client.send_text(json.dumps({
                    "topic": "/tracked_pose",
                    "data": pose_dict
                })), self.loop)
    
    def get_robot_transform(self):
        """使用 TF2 取得機器人在地圖中的位置和方向"""
        try:
            # 嘗試從 TF 取得轉換
            transform = self.tf_buffer.lookup_transform(
                'map', 
                'base_link', 
                rclpy.time.Time(),
                timeout=Duration(seconds=0.1)
            )
            
            # 更新機器人位置
            self.robot_x = transform.transform.translation.x
            self.robot_y = transform.transform.translation.y
            
            # 從四元數計算 yaw
            q = transform.transform.rotation
            siny_cosp = 2.0 * (q.w * q.z + q.x * q.y)
            cosy_cosp = 1.0 - 2.0 * (q.y * q.y + q.z * q.z)
            self.robot_yaw = math.atan2(siny_cosp, cosy_cosp)
            
            return True
        except Exception as ex:
            # TF 不可用時使用 tracked_pose 的資料
            # self.get_logger().warn_throttle(5.0, f"Could not get TF transform: {ex}")
            return False

    # --- Front LiDAR 控制 ---
    def enable_front_lidar(self):
        if not self.front_lidar_enabled:
            self.front_lidar_enabled = True
            self.front_lidar_sub = self.create_subscription(
                LaserScan, "/scan_1", self.front_lidar_callback, qos_profile_sensor_data)
            self.get_logger().info('[front_lidar] 訂閱已啟用')

    def disable_front_lidar(self):
        if self.front_lidar_enabled and self.front_lidar_sub:
            self.destroy_subscription(self.front_lidar_sub)
            self.front_lidar_sub = None
            self.front_lidar_enabled = False
            self.get_logger().info('[front_lidar] 訂閱已停用')

    def front_lidar_callback(self, msg):
        # Frame skipping for performance
        self.front_frame_counter += 1
        if self.front_frame_counter % max(1, self.front_frame_skip) != 0:
            return
        
        points = []
        
        # 取樣步長
        step = max(1, self.front_ray_step)
        
        target_frame = "map"
        laser_frame = msg.header.frame_id if msg.header.frame_id else target_frame
        
        try:
            # 使用最新的 TF（tf2::TimePointZero），而不是 msg 的時間戳
            transform = self.tf_buffer.lookup_transform(
                target_frame,
                laser_frame,
                rclpy.time.Time(),  # 最新的 TF
                timeout=Duration(seconds=0.1)
            )
            
            # 取得轉換矩陣
            trans = transform.transform.translation
            rot = transform.transform.rotation
            
            # 計算旋轉矩陣
            qx, qy, qz, qw = rot.x, rot.y, rot.z, rot.w
            
            # 四元數轉旋轉矩陣（只需要 2D 部分）
            r00 = 1 - 2*(qy*qy + qz*qz)
            r01 = 2*(qx*qy - qz*qw)
            r10 = 2*(qx*qy + qz*qw)
            r11 = 1 - 2*(qx*qx + qz*qz)
            
            angle = msg.angle_min
            for i in range(0, len(msg.ranges), step):
                r = msg.ranges[i]
                
                if not math.isfinite(r) or r < msg.range_min or r > msg.range_max:
                    angle += msg.angle_increment * step
                    continue
                
                # 1. LiDAR 座標系中的點
                lx = r * math.cos(angle)
                ly = r * math.sin(angle)
                
                # 2. 使用轉換矩陣轉到地圖座標系
                # p_map = tf_map_laser * p_laser
                x_map = trans.x + r00 * lx + r01 * ly
                y_map = trans.y + r10 * lx + r11 * ly
                
                points.append({"x": x_map, "y": y_map})
                angle += msg.angle_increment * step
                    
        except (LookupException, ConnectivityException, ExtrapolationException) as ex:
            self.get_logger().warn(f"Could not transform FRONT laser: {ex}")
            return
        
        for client in self.ws_clients:
            asyncio.run_coroutine_threadsafe(
                client.send_text(json.dumps({
                    "topic": "/front_lidar",
                    "data": {"points": points}
                })), self.loop)

    # --- Rear LiDAR 控制 ---
    def enable_rear_lidar(self):
        if not self.rear_lidar_enabled:
            self.rear_lidar_enabled = True
            self.rear_lidar_sub = self.create_subscription(
                LaserScan, "/scan_2", self.rear_lidar_callback, qos_profile_sensor_data)
            self.get_logger().info('[rear_lidar] 訂閱已啟用')

    def disable_rear_lidar(self):
        if self.rear_lidar_enabled and self.rear_lidar_sub:
            self.destroy_subscription(self.rear_lidar_sub)
            self.rear_lidar_sub = None
            self.rear_lidar_enabled = False
            self.get_logger().info('[rear_lidar] 訂閱已停用')

    def rear_lidar_callback(self, msg):
        # Frame skipping for performance
        self.rear_frame_counter += 1
        if self.rear_frame_counter % max(1, self.rear_frame_skip) != 0:
            return
        
        points = []
        step = max(1, self.rear_ray_step)
        
        target_frame = "map"
        laser_frame = msg.header.frame_id if msg.header.frame_id else target_frame
        
        try:
            # 使用最新的 TF
            transform = self.tf_buffer.lookup_transform(
                target_frame,
                laser_frame,
                rclpy.time.Time(),
                timeout=Duration(seconds=0.1)
            )
            
            trans = transform.transform.translation
            rot = transform.transform.rotation
            
            # 四元數轉旋轉矩陣
            qx, qy, qz, qw = rot.x, rot.y, rot.z, rot.w
            r00 = 1 - 2*(qy*qy + qz*qz)
            r01 = 2*(qx*qy - qz*qw)
            r10 = 2*(qx*qy + qz*qw)
            r11 = 1 - 2*(qx*qx + qz*qz)
            
            angle = msg.angle_min
            for i in range(0, len(msg.ranges), step):
                r = msg.ranges[i]
                
                if not math.isfinite(r) or r < msg.range_min or r > msg.range_max:
                    angle += msg.angle_increment * step
                    continue
                
                lx = r * math.cos(angle)
                ly = r * math.sin(angle)
                
                x_map = trans.x + r00 * lx + r01 * ly
                y_map = trans.y + r10 * lx + r11 * ly
                
                points.append({"x": x_map, "y": y_map})
                angle += msg.angle_increment * step
                    
        except (LookupException, ConnectivityException, ExtrapolationException) as ex:
            self.get_logger().warn(f"Could not transform REAR laser: {ex}")
            return
        
        for client in self.ws_clients:
            asyncio.run_coroutine_threadsafe(
                client.send_text(json.dumps({
                    "topic": "/rear_lidar",
                    "data": {"points": points}
                })), self.loop)

    # --- Global Path 控制 ---
    def enable_path(self):
        if not self.path_enabled:
            self.path_enabled = True
            self.path_sub = self.create_subscription(
                Path, "/plan", self.path_callback, 10)
            self.get_logger().info('[global_path] 訂閱已啟用')

    def disable_path(self):
        if self.path_enabled and self.path_sub:
            self.destroy_subscription(self.path_sub)
            self.path_sub = None
            self.path_enabled = False
            self.get_logger().info('[global_path] 訂閱已停用')

    def path_callback(self, msg):
        points = []
        for pose in msg.poses:
            points.append({
                "x": pose.pose.position.x,
                "y": pose.pose.position.y
            })
        
        for client in self.ws_clients:
            asyncio.run_coroutine_threadsafe(
                client.send_text(json.dumps({
                    "topic": "/global_path",
                    "data": {"points": points}
                })), self.loop)

    # --- Local Costmap 控制 ---
    def enable_costmap(self):
        if not self.costmap_enabled:
            self.costmap_enabled = True
            self.costmap_sub = self.create_subscription(
                CostmapMsg, "/local_costmap/costmap", self.costmap_callback, 10)
            self.get_logger().info('[local_costmap] 訂閱已啟用')

    def disable_costmap(self):
        if self.costmap_enabled and self.costmap_sub:
            self.destroy_subscription(self.costmap_sub)
            self.costmap_sub = None
            self.costmap_enabled = False
            self.get_logger().info('[local_costmap] 訂閱已停用')

    def costmap_callback(self, msg):
        points = []
        info = msg.info
        w, h = info.width, info.height
        data = msg.data
        
        if w == 0 or h == 0 or not data:
            return
        
        target_frame = "map"
        grid_frame = msg.header.frame_id if msg.header.frame_id else target_frame
        
        try:
            # 取得 grid frame 到 map 的轉換（使用最新 TF）
            tf_map_grid = self.tf_buffer.lookup_transform(
                target_frame,
                grid_frame,
                rclpy.time.Time(),
                timeout=Duration(seconds=0.1)
            )
            
            # grid frame 的轉換
            trans_grid = tf_map_grid.transform.translation
            rot_grid = tf_map_grid.transform.rotation
            
            # 四元數轉旋轉矩陣（grid frame）
            qx, qy, qz, qw = rot_grid.x, rot_grid.y, rot_grid.z, rot_grid.w
            r00_grid = 1 - 2*(qy*qy + qz*qz)
            r01_grid = 2*(qx*qy - qz*qw)
            r10_grid = 2*(qx*qy + qz*qw)
            r11_grid = 1 - 2*(qx*qx + qz*qz)
            
            # origin 的轉換（在 grid frame 中）
            origin_x = info.origin.position.x
            origin_y = info.origin.position.y
            
            # origin 的旋轉
            qx_o, qy_o, qz_o, qw_o = (info.origin.orientation.x, 
                                       info.origin.orientation.y,
                                       info.origin.orientation.z,
                                       info.origin.orientation.w)
            r00_origin = 1 - 2*(qy_o*qy_o + qz_o*qz_o)
            r01_origin = 2*(qx_o*qy_o - qz_o*qw_o)
            r10_origin = 2*(qx_o*qy_o + qz_o*qw_o)
            r11_origin = 1 - 2*(qx_o*qx_o + qz_o*qz_o)
            
            res = info.resolution
            
            # 只發送有障礙的點
            for y in range(h):
                for x in range(w):
                    idx = y * w + x
                    cost = data[idx]
                    
                    if cost <= 0:  # 跳過 unknown (-1) 和 free (0)
                        continue
                    
                    # 1. grid 座標系中的點（cell center）
                    gx = (x + 0.5) * res
                    gy = (y + 0.5) * res
                    
                    # 2. 先套用 origin 的轉換（在 grid frame 中）
                    wx = origin_x + r00_origin * gx + r01_origin * gy
                    wy = origin_y + r10_origin * gx + r11_origin * gy
                    
                    # 3. 再套用 grid frame 到 map 的轉換
                    mx = trans_grid.x + r00_grid * wx + r01_grid * wy
                    my = trans_grid.y + r10_grid * wx + r11_grid * wy
                    
                    points.append({"x": mx, "y": my, "cost": int(cost)})
            
            self.get_logger().info(f"Local costmap: {w}x{h}, occupied cells: {len(points)}", 
                                   throttle_duration_sec=2.0)
                    
        except (LookupException, ConnectivityException, ExtrapolationException) as ex:
            self.get_logger().warn(f"Could not transform local costmap: {ex}", 
                                   throttle_duration_sec=2.0)
            return
        
        for client in self.ws_clients:
            asyncio.run_coroutine_threadsafe(
                client.send_text(json.dumps({
                    "topic": "/local_costmap",
                    "data": {"points": points}
                })), self.loop)
    # def listener_callback(self, msg):
    #     data = msg.data
    #     print(f"[ROS] 接收到: {data}")
    #     for client in self.ws_clients:
    #         asyncio.run_coroutine_threadsafe(
    #             client.send_text(f"[ROS] 狀態: {data}"),
    #             self.loop 
    #         )
    def publish_to_route_ctrl_event(self, data: str):
        msg = String()
        msg.data = json.dumps(data)
        self.publisher_route_ctrl.publish(msg)
        self.get_logger().info(f'[route_ctrl]發送資料: {msg.data}')

    def publish_to_ui_event(self, data: str):
        msg = String()
        msg.data = json.dumps(data)
        self.publisher_ui.publish(msg)
        self.get_logger().info(f'[ui]發送資料: {msg.data}')


@app.get("/tables")
def get_table_names():
    try:
        # 1. 讀取 lastmap
        with open("/home/ubuntu/sambashare/lastmap", "r") as f:
            map_name = f.read().strip()
        

        # 2. 讀取 path.json
        json_path = f"/home/ubuntu/sambashare/{map_name}/path.json"
        with open(json_path, "r") as f:
            path_data = json.load(f)
        
        # 3. 取出所有 type == table 的 name
        tables = []
        for p in path_data.get("point", {}).values():
            if p.get("type") == "table":
                tables.append(p.get("name"))
        tables.sort()  # 依照字典序排序

        return JSONResponse(content={"tables": tables})
    except Exception as e:
        return JSONResponse(content={"error": str(e)}, status_code=500)


@app.get("/map_live_image")
def get_map_live_image():
    try:
        # use ros_node.live_map_path written by map_callback
        path = getattr(ros_node, "live_map_path", "/tmp/live_map.png")
        response = FileResponse(path, media_type="image/png")
        # 防止瀏覽器快取
        response.headers["Cache-Control"] = "no-cache, no-store, must-revalidate"
        response.headers["Pragma"] = "no-cache"
        response.headers["Expires"] = "0"
        return response
    except Exception as e:
        return JSONResponse(content={"error": str(e)}, status_code=500)


@app.get("/map_image")
def get_map_image():
    try:
        with open("/home/ubuntu/sambashare/lastmap", "r") as f:
            map_name = f.read().strip()
        img_path = f"/home/ubuntu/sambashare/{map_name}/WHEELTEC.png"
        response = FileResponse(img_path, media_type="image/png")
        # 防止瀏覽器快取
        response.headers["Cache-Control"] = "no-cache, no-store, must-revalidate"
        response.headers["Pragma"] = "no-cache"
        response.headers["Expires"] = "0"
        return response
    except Exception as e:
        return JSONResponse(content={"error": str(e)}, status_code=500)


@app.get("/map_meta")
def get_map_meta():
    try:
        with open("/home/ubuntu/sambashare/lastmap", "r") as f:
            map_name = f.read().strip()
        yaml_path = f"/home/ubuntu/sambashare/{map_name}/WHEELTEC.yaml"
        img_path = f"/home/ubuntu/sambashare/{map_name}/WHEELTEC.png"

        with open(yaml_path, "r") as f:
            yml = yaml.safe_load(f)
        resolution = yml["resolution"]
        origin = yml["origin"]   # [origin_x, origin_y, theta]

        # 用 PIL 取得圖片 pixel 尺寸
        img = Image.open(img_path)
        width, height = img.size

        # 回傳格式
        import time
        timestamp = int(time.time() * 1000)  # 毫秒級時間戳
        return {
            "width": width,
            "height": height,
            "resolution": resolution,
            "origin": origin[:2],  # 只要 x, y
            "image": f"/map_image?ts={timestamp}"  # 動態時間戳防止快取
        }
    except Exception as e:
        return JSONResponse(content={"error": str(e)}, status_code=500)


@app.get("/poi_data")
def get_poi_data():
    try:
        with open("/home/ubuntu/sambashare/lastmap", "r") as f:
            map_name = f.read().strip()
        path_json = f"/home/ubuntu/sambashare/{map_name}/path.json"
        
        with open(path_json, "r") as f:
            poi_data = json.load(f)
        
        return JSONResponse(content=poi_data)
    except Exception as e:
        return JSONResponse(content={"error": str(e)}, status_code=500)



@app.on_event("startup")
async def startup_event():
    global ros_node
    global publisher_node
    rclpy.init()
    ros_node = StatusSubscriber(loop) 


    def ros_spin():
        import time
        print("rclpy spin_once sleep interval=", ros_node.spin_timeout)
        while ros_node.spin_running and rclpy.ok():
            rclpy.spin_once(ros_node, timeout_sec=0.001)  # 1ms
            time.sleep(ros_node.spin_timeout)  # 使用 spin_timeout 作為 sleep 間隔 
    loop.run_in_executor(None, ros_spin)


@app.get("/spin_config")
def get_spin_config():
    """取得目前的 ROS 處理間隔參數"""
    return JSONResponse(content={
        "timeout_ms": ros_node.spin_timeout * 1000
    })


@app.post("/spin_config")
async def set_spin_config(timeout_ms: float = 10.0):
    """設定 ROS 處理間隔（毫秒）"""
    # 限制範圍 0ms ~ 100ms
    timeout_ms = max(0.0, min(100.0, timeout_ms))
    ros_node.spin_timeout = timeout_ms / 1000.0
    return JSONResponse(content={
        "status": "ok",
        "timeout_ms": timeout_ms
    })  

@app.websocket("/ws")
async def websocket_endpoint(websocket: WebSocket):
    await websocket.accept()
    ros_node.ws_clients.append(websocket)
    try:
        while True:
            data = await websocket.receive_text()
            try:
                json_data = json.loads(data)
                topic = json_data.get("topic")
                content = json_data.get("data")
                print(f"[Web] received:[{topic}] {content}")
                if topic == "/route_ctrl_event":
                    ros_node.publish_to_route_ctrl_event(content)
                    await websocket.send_text(json.dumps({
                        "topic": "system",
                        "data": f"Sent: {content}"
                    }))
                # 可以再加更多 topic...
                elif topic == "/ui_event":
                    ros_node.publish_to_ui_event(content)
                    await websocket.send_text(json.dumps({
                        "topic": "system",
                        "data": f"Sent: {content}"
                    }))
                # --- 新增: 控制 /tracked_pose 訂閱 ---
                elif topic == "control_tracked_pose":
                    if content == "start":
                        ros_node.enable_tracked_pose()
                        await websocket.send_text(json.dumps({
                            "topic": "system",
                            "data": "Tracked_pose 訂閱啟用"
                        }))
                    elif content == "stop":
                        ros_node.disable_tracked_pose()
                        await websocket.send_text(json.dumps({
                            "topic": "system",
                            "data": "Tracked_pose 訂閱停用"
                        }))
                    else:
                        await websocket.send_text(json.dumps({
                            "topic": "error",
                            "data": "未知控制指令"
                        }))
                # --- 控制 LiDAR 訂閱 ---
                elif topic == "control_lidar":
                    lidar_type = content.get("type")
                    enabled = content.get("enabled", False)
                    if lidar_type == "front":
                        if enabled:
                            ros_node.enable_front_lidar()
                        else:
                            ros_node.disable_front_lidar()
                        await websocket.send_text(json.dumps({
                            "topic": "system",
                            "data": f"Front LiDAR {'啟用' if enabled else '停用'}"
                        }))
                    elif lidar_type == "rear":
                        if enabled:
                            ros_node.enable_rear_lidar()
                        else:
                            ros_node.disable_rear_lidar()
                        await websocket.send_text(json.dumps({
                            "topic": "system",
                            "data": f"Rear LiDAR {'啟用' if enabled else '停用'}"
                        }))
                # --- 控制 Global Path 訂閱 ---
                elif topic == "control_path":
                    enabled = content.get("enabled", False)
                    if enabled:
                        ros_node.enable_path()
                    else:
                        ros_node.disable_path()
                    await websocket.send_text(json.dumps({
                        "topic": "system",
                        "data": f"Global Path {'啟用' if enabled else '停用'}"
                    }))
                # --- 控制 Local Costmap 訂閱 ---
                elif topic == "control_costmap":
                    enabled = content.get("enabled", False)
                    if enabled:
                        ros_node.enable_costmap()
                    else:
                        ros_node.disable_costmap()
                    await websocket.send_text(json.dumps({
                        "topic": "system",
                        "data": f"Local Costmap {'啟用' if enabled else '停用'}"
                    }))
                # --- Set Initial Pose ---
                elif topic == "set_initial_pose":
                    x = content.get("x", 0)
                    y = content.get("y", 0)
                    yaw = content.get("yaw", 0)
                    print(f"[WebSocket] 收到 set_initial_pose: x={x}, y={y}, yaw={yaw}")
                    ros_node.send_initial_pose(x, y, yaw)
                    await websocket.send_text(json.dumps({
                        "topic": "system",
                        "data": f"Initial Pose 已設定: x={x:.2f}, y={y:.2f}, yaw={yaw:.2f}"
                    }))
                # --- Nav2 座標導航 ---
                elif topic == "navigate_to_pose":
                    x = content.get("x", 0)
                    y = content.get("y", 0)
                    yaw = content.get("yaw", 0)
                    print(f"[WebSocket] 收到 navigate_to_pose: x={x}, y={y}, yaw={yaw}")
                    ros_node.send_goal_pose(x, y, yaw)
                    await websocket.send_text(json.dumps({
                        "topic": "system",
                        "data": f"導航目標已設定: x={x:.2f}, y={y:.2f}, yaw={yaw:.2f}"
                    }))
                # --- 取消 Nav2 導航 ---
                elif topic == "cancel_navigation":
                    print("[WebSocket] 收到 cancel_navigation")
                    ros_node.cancel_navigation()
                    await websocket.send_text(json.dumps({
                        "topic": "system",
                        "data": "導航已取消"
                    }))
                elif topic == "/remote_control":
                    linear_x = content.get("linear_x", 0)
                    angular_z = content.get("angular_z", 0)
                    ros_node.send_cmd_vel(linear_x, angular_z)
                    # await websocket.send_text(json.dumps({
                    #     "topic": "system",
                    #     "data": f"cmd_vel sent: linear_x={linear_x}, angular_z={angular_z}"
                    # })) 
                elif topic == "/stop_motor":
                    data_value = content.get("data", False)
                    ros_node.send_stop_motor(data_value)
                    # await websocket.send_text(json.dumps({
                    #     "topic": "system",
                    #     "data": f"Sent /stop_motor: {data_value}"
                    # }))
                # --- Ping/Pong for latency measurement ---
                elif topic == "ping":
                    await websocket.send_text(json.dumps({
                        "topic": "pong",
                        "data": content
                    }))
                else:
                    await websocket.send_text(json.dumps({
                        "topic": "system",
                        "data": f"Unknown topic: {topic}"
                    }))
            except Exception as e:
                await websocket.send_text(json.dumps({
                    "topic": "error",
                    "data": str(e)
                }))
    except Exception as e:
        print(f"[WebSocket] 連線中斷: {e}")
    finally:
        ros_node.ws_clients.remove(websocket)