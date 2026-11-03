package assertions

// PrivateFile requires a regular file accessible only to its owner: exact 0600
// permissions on POSIX, or a protected, owner-only full-control DACL on Windows.
func PrivateFile(t fatalReporter, path string) {
	t.Helper()
	if err := privateFileError(path); err != nil {
		t.Fatalf("expected owner-private file %q: %v", path, err)
	}
}
