package extract

// TextForTest exposes the visible-text extraction to this package's tests.
//
// Exported for tests only, and from a file that says so: the boundary rules it
// implements are worth asserting directly, and the alternative is asserting them
// through a full extraction where a failure could be any of five rungs.
func TextForTest(raw []byte) string { return textOf(raw) }
