package ws

import "testing"

// methodCoverage documents the personal backend's coverage of every RPC method
// the device's dispatcher (athenad + sunnylinkd) advertises. The two roles are:
//
//  1. invoker -- a Call<Method> wrapper that the server uses to send a request
//     to the device when the operator UI needs the data.
//  2. handler -- a server-side MethodHandler for unprompted requests the
//     device pushes to us (e.g. forwardLogs, storeStats). Most methods do not
//     need a handler because the device never initiates them.
//
// When a new device-side dispatcher method lands in athenad/sunnylinkd, add it
// here with the right coverage flags and either implement the missing piece or
// document why we do not need it.
type methodCoverage struct {
	method string
	// hasInvoker is true when the personal backend has a Go-side wrapper for
	// the server to send this RPC to the device. Methods we never call on the
	// device (e.g. forwardLogs is device-initiated) leave this false.
	hasInvoker bool
	// requiresHandler is true when the device sends this method unprompted, so
	// the backend must dispatch a server-side handler. forwardLogs and
	// storeStats are the only two in this category as of writing.
	requiresHandler bool
	// note explains anything non-obvious -- in particular, why a method
	// without an invoker is intentional rather than a gap.
	note string
}

// athenadMethods is the snapshot of every @dispatcher.add_method or
// dispatcher[<name>] = ... in system/athena/athenad.py at the time of writing.
// Source-of-truth lives in the device repo; keep this list in sync when
// rebasing.
var athenadMethods = []methodCoverage{
	{method: "getMessage", hasInvoker: true},
	{method: "getVersion", hasInvoker: true},
	{method: "setNavDestination", hasInvoker: true},
	{method: "listDataDirectory", hasInvoker: true},
	{method: "uploadFileToUrl", hasInvoker: true},
	{method: "uploadFilesToUrls", hasInvoker: true},
	{method: "listUploadQueue", hasInvoker: true},
	{method: "cancelUpload", hasInvoker: true},
	{method: "setRouteViewed", hasInvoker: true},
	{method: "getPublicKey", hasInvoker: true},
	{method: "getSshAuthorizedKeys", hasInvoker: true},
	{method: "getGithubUsername", hasInvoker: true},
	{method: "getSimInfo", hasInvoker: true},
	{method: "getNetworkType", hasInvoker: true},
	{method: "getNetworkMetered", hasInvoker: true},
	{method: "getNetworks", hasInvoker: true},
	{method: "takeSnapshot", hasInvoker: true},
	{
		method:     "startLocalProxy",
		hasInvoker: false,
		note:       "deferred to P1b: requires a secondary WS endpoint + binary frame proxy for SSH tunneling",
	},
}

// sunnylinkMethods covers the sunnypilot-only methods registered in
// sunnypilot/sunnylink/athena/sunnylinkd.py.
var sunnylinkMethods = []methodCoverage{
	{method: "toggleLogUpload", hasInvoker: true},
	{method: "getParamsAllKeys", hasInvoker: true},
	{method: "getParamsAllKeysV1", hasInvoker: true},
	{method: "getParamsMetadata", hasInvoker: true},
	{method: "getParams", hasInvoker: true},
	{method: "saveParams", hasInvoker: true},
}

// devicePushedMethods are the JSON-RPC methods the device sends unprompted.
// Each requires a server-side MethodHandler registered in cmd/server/routes.go.
var devicePushedMethods = []methodCoverage{
	{method: "forwardLogs", requiresHandler: true},
	{method: "storeStats", requiresHandler: true},
}

// TestRPCCoverage_NoSilentGaps asserts every entry that has neither an invoker
// nor a handler carries an explanatory note. This is the regression guard
// against silently dropping a method when rebasing against athenad.
func TestRPCCoverage_NoSilentGaps(t *testing.T) {
	for _, m := range append(append(athenadMethods, sunnylinkMethods...), devicePushedMethods...) {
		if !m.hasInvoker && !m.requiresHandler && m.note == "" {
			t.Errorf("method %q has neither invoker nor handler nor explanatory note", m.method)
		}
	}
}

// TestRPCCoverage_DevicePushedHandlersWired asserts that every method marked
// requiresHandler is registered in the production handlers map. The map is
// constructed by cmd/server/routes.go; this test covers the package-level
// factories directly so a missing wiring shows up at unit-test time rather
// than at first device contact.
func TestRPCCoverage_DevicePushedHandlersWired(t *testing.T) {
	store := &memDeviceLogStorage{}
	handlers := map[string]MethodHandler{
		"forwardLogs": MakeForwardLogsHandler(store, fixedIDFunc()),
		"storeStats":  MakeStoreStatsHandler(store, fixedIDFunc()),
	}

	for _, m := range devicePushedMethods {
		if _, ok := handlers[m.method]; !ok {
			t.Errorf("device-pushed method %q has no registered handler", m.method)
		}
	}
}
