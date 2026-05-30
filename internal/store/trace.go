// Package store provides persistent storage implementations for Tasks and StepTraces.
//
// StepTrace persistence is handled inline within SaveFullTask via the ReplaceTraces method
// on each store implementation (e.g. SQLiteStore.ReplaceTraces). There is no separate
// Trace-only interface method by design: traces are always loaded/saved as part of their
// parent Task to maintain consistency.
package store
