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
}

// LoadDocuments reads the document storage configuration from the environment,
// reporting every problem it finds at once.
func LoadDocuments() (Documents, error) {
	var e env

	d := Documents{
		StorageRoot:    e.optionalString("DOCUMENT_STORAGE_ROOT", DefaultDocumentStorageRoot),
		MaxUploadBytes: e.optionalInt64("DOCUMENT_MAX_UPLOAD_BYTES", 10<<20), // 10 MiB
	}

	if d.MaxUploadBytes < 1 {
		e.fail("DOCUMENT_MAX_UPLOAD_BYTES must be at least 1, got %d", d.MaxUploadBytes)
	}

	if err := e.err(); err != nil {
		return Documents{}, err
	}
	return d, nil
}

// String renders the configuration for startup logging.
func (d Documents) String() string {
	return fmt.Sprintf("storage_root=%s max_upload_bytes=%d", d.StorageRoot, d.MaxUploadBytes)
}
