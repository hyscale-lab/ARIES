package hermes

// Protocol state remains shared; common runtime data and credentials are private.
func snapshotSession(active *session) *session {
	copy := *active
	copy.Occurrence = active.Occurrence.Snapshot()
	return &copy
}
