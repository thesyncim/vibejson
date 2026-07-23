//go:build slopjson_validate_hooks

package slopjson

// The slopjson_validate_hooks build tag is a test/debug integrity mode. It
// validates exactly the bytes emitted by each MarshalerSimd invocation. The
// production build compiles this branch away and retains unchecked splicing.
const validateSimdHookOutput = true
