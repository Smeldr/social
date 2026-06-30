package social

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"

	"smeldr.dev/core"
)

// Route associates a lifecycle signal and content type with an agent URL.
// When Smeldr fires the matching signal for the matching content type, the
// Router enqueues an outbound HTTP POST to AgentURL.
//
// Use the builder functions [OnPublish], [OnSchedule], [OnArchive], and
// [OnDelete] to construct Route values.
type Route struct {
	// Signal is the Smeldr lifecycle signal this route responds to.
	Signal smeldr.LifecycleEvent

	// ContentType is the Go type name of the content type (e.g. "Post", "Story").
	// Matching is exact. Use the PascalCase struct name exactly as it appears
	// in your content type definition.
	ContentType string

	// AgentURL is the HTTPS endpoint that receives the signed JSON payload.
	// Validated at [AddRoutes] time — must be HTTPS and not a private address.
	AgentURL string
}

// OnPublish returns a [Route] that fires on [smeldr.AfterPublish] for the
// named content type. agentURL must be a public HTTPS URL (validated at
// [AddRoutes] time).
func OnPublish(contentType, agentURL string) Route {
	return Route{Signal: smeldr.AfterPublish, ContentType: contentType, AgentURL: agentURL}
}

// OnSchedule returns a [Route] that fires on [smeldr.AfterSchedule] for the
// named content type.
func OnSchedule(contentType, agentURL string) Route {
	return Route{Signal: smeldr.AfterSchedule, ContentType: contentType, AgentURL: agentURL}
}

// OnArchive returns a [Route] that fires on [smeldr.AfterArchive] for the
// named content type.
func OnArchive(contentType, agentURL string) Route {
	return Route{Signal: smeldr.AfterArchive, ContentType: contentType, AgentURL: agentURL}
}

// OnDelete returns a [Route] that fires on [smeldr.AfterDelete] for the
// named content type.
func OnDelete(contentType, agentURL string) Route {
	return Route{Signal: smeldr.AfterDelete, ContentType: contentType, AgentURL: agentURL}
}

// validateRoute panics if the route is structurally invalid. Called once at
// [AddRoutes] time so misconfiguration is caught immediately at startup, before
// any request is served — consistent with how [New] panics on a nil db or
// empty secret.
func validateRoute(r Route) {
	if r.ContentType == "" {
		panic("social.AddRoutes: Route.ContentType must not be empty")
	}
	// Warn if ContentType does not start with an uppercase letter — the
	// developer likely passed a lowercase string instead of the struct name.
	first := rune(r.ContentType[0])
	if !unicode.IsUpper(first) {
		panic(fmt.Sprintf(
			"social.AddRoutes: Route.ContentType %q should be PascalCase (the Go struct name, e.g. %q)",
			r.ContentType, strings.ToUpper(r.ContentType[:1])+r.ContentType[1:],
		))
	}
	if err := validateAgentURL(r.AgentURL); err != nil {
		panic(fmt.Sprintf("social.AddRoutes: %v", err))
	}
}

// privateRanges are the CIDR blocks that must not be targeted by agent URLs.
// These cover loopback, link-local, and RFC 1918 private ranges.
var privateRanges = func() []*net.IPNet {
	cidrs := []string{
		"127.0.0.0/8",    // loopback
		"::1/128",        // IPv6 loopback
		"10.0.0.0/8",     // RFC 1918
		"172.16.0.0/12",  // RFC 1918
		"192.168.0.0/16", // RFC 1918
		"169.254.0.0/16", // link-local
		"fc00::/7",       // IPv6 unique local
		"fe80::/10",      // IPv6 link-local
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, _ := net.ParseCIDR(c)
		nets = append(nets, n)
	}
	return nets
}()

// validateAgentURL returns an error if agentURL is not a valid, public HTTPS URL.
// It rejects non-HTTPS schemes, .local hostnames, and URLs that resolve to
// private or loopback IP ranges.
func validateAgentURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("Route.AgentURL must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("Route.AgentURL %q: invalid URL: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("Route.AgentURL %q: scheme must be https", raw)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("Route.AgentURL %q: missing host", raw)
	}
	if strings.HasSuffix(host, ".local") || host == "localhost" {
		return fmt.Errorf("Route.AgentURL %q: local hostnames are not allowed", raw)
	}
	// Resolve and check IPs. On servers where DNS is unavailable at startup
	// we skip the IP check — SSRF protection via hostname is still enforced above.
	addrs, err := net.LookupHost(host)
	if err != nil {
		// Cannot resolve at registration time — allow it; the worker will
		// fail at delivery time with a network error (treated as transient).
		return nil
	}
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			continue
		}
		for _, priv := range privateRanges {
			if priv.Contains(ip) {
				return fmt.Errorf("Route.AgentURL %q: resolves to private/loopback address %s", raw, addr)
			}
		}
	}
	return nil
}
