package config

import "fmt"

// DefaultDocumentStorageRoot is where uploaded files live when nothing says
// otherwise. It is a path inside the container; a developer running the binary
// directly will want to override it.
const DefaultDocumentStorageRoot = "/var/lib/incident_diag/documents"

// Documents is the configuration for uploaded document files.
//
// It is loaded by the API, which writes these files, and by the ingestion
// worker, which reads them. Both mount the same volume, so both need the same
// root; neither should have to know the other's configuration.
type Documents struct {
	StorageRoot string

	// MaxUploadBytes caps a single uploaded file. It is a file-size limit, not
	// a request-size limit: the multipart envelope around the file is bounded
	// separately.
	MaxUploadBytes int64

	// ChunkTargetTokens is the budget one chunk is packed against, as an
	// estimate over runes rather than a real tokenizer's count. It is a target
	// rather than a ceiling: a single packing unit larger than it becomes an
	// oversized chunk rather than being split.
	ChunkTargetTokens int

	// ChunkMaxPerDocument caps how many chunks one document may produce, so
	// that one pathological upload cannot consume the whole embedding budget.
	ChunkMaxPerDocument int
}

// LoadDocuments reads the document storage configuration from the environment,
// reporting every problem it finds at once.
func LoadDocuments() (Documents, error) {
	var e env

	d := Documents{
		StorageRoot:         e.optionalString("DOCUMENT_STORAGE_ROOT", DefaultDocumentStorageRoot),
		MaxUploadBytes:      e.optionalInt64("DOCUMENT_MAX_UPLOAD_BYTES", 10<<20), // 10 MiB
		ChunkTargetTokens:   e.optionalInt("CHUNK_TARGET_TOKENS", 400),
		ChunkMaxPerDocument: e.optionalInt("CHUNK_MAX_PER_DOCUMENT", 2000),
	}

	if d.MaxUploadBytes < 1 {
		e.fail("DOCUMENT_MAX_UPLOAD_BYTES must be at least 1, got %d", d.MaxUploadBytes)
	}
	if d.ChunkTargetTokens < 1 {
		e.fail("CHUNK_TARGET_TOKENS must be at least 1, got %d", d.ChunkTargetTokens)
	}
	if d.ChunkMaxPerDocument < 1 {
		e.fail("CHUNK_MAX_PER_DOCUMENT must be at least 1, got %d", d.ChunkMaxPerDocument)
	}

	if err := e.err(); err != nil {
		return Documents{}, err
	}
	return d, nil
}

// String renders the configuration for startup logging.
func (d Documents) String() string {
	return fmt.Sprintf("storage_root=%s max_upload_bytes=%d chunk_target_tokens=%d chunk_max_per_document=%d",
		d.StorageRoot, d.MaxUploadBytes, d.ChunkTargetTokens, d.ChunkMaxPerDocument)
}
