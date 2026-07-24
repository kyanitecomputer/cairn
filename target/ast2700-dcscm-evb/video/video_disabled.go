// No-op display stubs for the default build (video feature disabled). Building
// with the `ast2700video` tag replaces these with the real GFX/DisplayPort
// console (see video.go).

//go:build !ast2700video

package video

import "src.kyanite.computer/core/console"

// Init is a no-op unless the ast2700video build tag is set.
func Init() {}

// PollHotplug is a no-op unless the ast2700video build tag is set.
func PollHotplug() {}

// Commands returns no console commands unless the ast2700video build tag is set.
func Commands() []console.Command { return nil }
