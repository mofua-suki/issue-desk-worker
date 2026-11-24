package journal

// ContinuationSource identifies exactly the provenance abandoned by a retry.
// A later change to that job's generation or artifacts must be checked anew.
type ContinuationSource struct {
	ID         int64  `json:"id"`
	Generation int    `json:"generation"`
	Branch     string `json:"branch"`
	Head       string `json:"head"`
	PR         int64  `json:"pr"`
}

func SourceOf(r Record) ContinuationSource {
	return ContinuationSource{ID: r.ID, Generation: r.Generation, Branch: r.Branch, Head: r.Head, PR: r.PR}
}
