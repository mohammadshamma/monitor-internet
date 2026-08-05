package main

// supportedPlatform gates the runtime check in main.
//
// The tool is macOS-only: topology discovery shells out to /sbin/route and
// /usr/sbin/traceroute, and the agents are launchd LaunchAgents. It is not
// guarded with a //go:build constraint on the command itself, because that
// would make `go install` fail on other platforms with the cryptic "build
// constraints exclude all Go files" error. Building everywhere and failing
// clearly at startup is the friendlier contract.
const supportedPlatform = true
