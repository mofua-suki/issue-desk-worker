package journal

// RejectedStart is local evidence that the initial state checkpoint was
// refused before the implementation provider could be called. It is never
// included in workerExecution and therefore cannot be inferred on another PC.
type RejectedStart struct {
	JobID      int64 `json:"jobId"`
	Generation int   `json:"generation"`
	Run        int   `json:"run"`
	Epoch      int64 `json:"epoch"`
}
