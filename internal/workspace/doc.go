// Package workspace preserves an early standalone agent runtime prototype.
//
// Despite the directory name, this package is not a filesystem sandbox and is
// not wired into cmd/server. Production task execution lives in
// internal/orchestrator, tool registration in internal/tools, and filesystem
// authorization in internal/policy. New code must use those packages so policy,
// approval, persistence, cancellation, and budget controls are not bypassed.
//
// Deprecated: retained temporarily for source compatibility and historical
// reference. Do not add new integrations to this package.
package workspace
