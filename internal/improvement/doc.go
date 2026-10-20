// Package improvement implements the harnessing-level recursion of recursive
// harnessing: a persistent improvement harness that retains adopted harnesses
// and evidence-supported lessons from each task to guide future refinement.
//
// It is deliberately separate from internal/brain: brain stores compiled
// business knowledge (claims/evidence); the improvement harness stores
// knowledge about how to improve harnesses. Both follow the same principles:
// content-addressed records, append-only history, and logical retraction
// (a retracted lesson stays on disk but is excluded from reads), never
// physical erasure.
//
// On-disk layout under the store root:
//
//	lessons/<lesson-id>.json       one file per lesson, written atomically
//	retractions/<lesson-id>.json   presence means the lesson is retracted
//
// There is no central index file to corrupt: List scans the lessons directory
// and filters retracted entries. Writes are atomic (temp file + rename), so
// concurrent readers never see partial records.
package improvement
