package task

// MarshalStored renders the exact task-file payload Store.Put writes, without
// touching UpdatedAt. Callers that need semantic no-op detection for pushed
// tasks should compare this output rather than raw Task structs, because YAML
// omitempty coalesces nil/empty slices and maps on disk.
func MarshalStored(t Task) ([]byte, error) {
	return marshalTask(t, false)
}
