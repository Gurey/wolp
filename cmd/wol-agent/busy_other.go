//go:build !linux

package main

import "time"

// The busy checks are Linux-only for now (Windows comes later). On other
// systems the agent builds for local development and always reports idle.

func busyReasons(cfg Config) []string { return nil }

func waitForGPUIdle(timeout time.Duration) []string { return nil }
