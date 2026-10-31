// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package urlx

//go:generate optgen generate

// DefaultAllowedSchemes returns the schemes [ParseBaseURL] accepts by default:
// "http" and "https". It returns a fresh slice on every call.
func DefaultAllowedSchemes() []string {
	return []string{"http", "https"}
}

// options configures [ParseBaseURL].
type options struct {
	// allowedSchemes lists the accepted schemes. Matching is
	// case-insensitive: [url.Parse] lower-cases the parsed scheme and the
	// option lower-cases the configured ones.
	allowedSchemes []string `optgen:"default=DefaultAllowedSchemes()" optval:"lower,nonempty"`
	// endpointMode keeps the path exactly as given, trailing slash included,
	// for a URL that names a full endpoint. Without it (base mode) trailing
	// slashes are trimmed so that paths can be appended with [JoinPath].
	endpointMode bool
}
