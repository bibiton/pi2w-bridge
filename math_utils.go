package main

import "math"

// QuatToYaw converts quaternion (x, y, z, w) to yaw angle in radians.
func QuatToYaw(x, y, z, w float64) float64 {
	siny := 2.0 * (w*z + x*y)
	cosy := 1.0 - 2.0*(y*y+z*z)
	return math.Atan2(siny, cosy)
}

// QuatToYawDeg converts quaternion to yaw angle in degrees [0, 360).
func QuatToYawDeg(x, y, z, w float64) float64 {
	rad := QuatToYaw(x, y, z, w)
	deg := rad * 180.0 / math.Pi
	if deg < 0 {
		deg += 360.0
	}
	return deg
}
