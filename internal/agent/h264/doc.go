// Package h264 is the agent's software H.264 encoder: openh264, statically
// linked, emitting Constrained Baseline Annex-B that browsers decode in WebRTC.
// Builds without the openh264 tag get a stub whose New always fails.
package h264
