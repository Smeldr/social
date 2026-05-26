package forgesocial_test

import (
	"testing"

	forgesocial "smeldr.dev/social"
)

func TestValidateAgentURL_AcceptsPublicHTTPS(t *testing.T) {
	// A hostname that does not resolve in test environments will be allowed
	// (we skip IP check when DNS lookup fails). Use a clearly non-existent domain
	// so the test never makes a real network call.
	route := forgesocial.OnPublish("Post", "https://not-a-real-host-xyz.example.com/hook")
	if route.AgentURL == "" {
		t.Error("route.AgentURL should not be empty")
	}
}

func TestValidateAgentURL_RejectsHTTP(t *testing.T) {
	defer expectPanic(t, "http scheme should be rejected")
	forgesocial.ValidateAgentURLForTest("http://example.com/hook")
}

func TestValidateAgentURL_RejectsEmpty(t *testing.T) {
	defer expectPanic(t, "empty URL should be rejected")
	forgesocial.ValidateAgentURLForTest("")
}

func TestValidateAgentURL_RejectsLocalhost(t *testing.T) {
	defer expectPanic(t, "localhost should be rejected")
	forgesocial.ValidateAgentURLForTest("https://localhost/hook")
}

func TestValidateAgentURL_RejectsDotLocal(t *testing.T) {
	defer expectPanic(t, ".local hostname should be rejected")
	forgesocial.ValidateAgentURLForTest("https://myserver.local/hook")
}

func TestAddRoutes_PanicsOnLowercaseContentType(t *testing.T) {
	defer expectPanic(t, "lowercase contentType should panic")
	forgesocial.OnPublish("post", "https://agent.example.com/hook")
	// validateRoute is called inside AddRoutes, not OnPublish.
	// Test the validation directly via an exported helper.
	forgesocial.ValidateRouteForTest(forgesocial.Route{
		Signal:      "after_publish",
		ContentType: "post",
		AgentURL:    "https://agent.example.com/hook",
	})
}

func TestAddRoutes_PanicsOnEmptyContentType(t *testing.T) {
	defer expectPanic(t, "empty contentType should panic")
	forgesocial.ValidateRouteForTest(forgesocial.Route{
		Signal:      "after_publish",
		ContentType: "",
		AgentURL:    "https://agent.example.com/hook",
	})
}

func TestRouteBuilders_SignalAssignment(t *testing.T) {
	tests := []struct {
		name    string
		route   forgesocial.Route
		wantSig string
	}{
		{"OnPublish", forgesocial.OnPublish("Post", "https://x.example.com/h"), "after_publish"},
		{"OnSchedule", forgesocial.OnSchedule("Post", "https://x.example.com/h"), "after_schedule"},
		{"OnArchive", forgesocial.OnArchive("Post", "https://x.example.com/h"), "after_archive"},
		{"OnDelete", forgesocial.OnDelete("Post", "https://x.example.com/h"), "after_delete"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.route.Signal) != tc.wantSig {
				t.Errorf("Signal = %q; want %q", tc.route.Signal, tc.wantSig)
			}
			if tc.route.ContentType != "Post" {
				t.Errorf("ContentType = %q; want %q", tc.route.ContentType, "Post")
			}
		})
	}
}

// expectPanic is a test helper that fails the test if no panic occurs.
func expectPanic(t *testing.T, msg string) {
	t.Helper()
	if r := recover(); r == nil {
		t.Errorf("expected panic: %s", msg)
	}
}
