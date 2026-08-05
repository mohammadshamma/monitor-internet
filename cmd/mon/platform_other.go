//go:build !darwin

package main

// See platform_darwin.go for why this is a runtime check rather than a build
// constraint on the whole command.
const supportedPlatform = false
