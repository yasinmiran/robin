// Package h264 is the agent's software H.264 codec: openh264, statically
// linked, encoding Constrained Baseline Annex-B that browsers decode in
// WebRTC, and decoding the browser's camera. Builds without the openh264
// tag get a stub whose New and NewDecoder always fail.
package h264
