package api

import "github.com/wpan36/incident_diag/internal/store"

// Incident is the wire shape of an incident.
//
// One shape serves both the listing and the fetch. A list item is not a reduced
// projection of the fetched resource: the front end then has one type per
// entity instead of two, and nothing has to be re-fetched just to render a
// detail view. Neither entity is large enough for the saved bytes to matter.
//
// Note what is not here: no omitempty on Service. A nullable column renders as
// an explicit null, never an absent key, so the set of keys is the same in
// every response. That is what makes the generated TypeScript types in M28
// honest — omitempty would make service optional in the type system for a
// reason that has nothing to do with the domain, and would make an empty string
// and an absent value indistinguishable on the wire.
type Incident struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Service     *string `json:"service"`
	CreatedAt   Time    `json:"created_at"`
	UpdatedAt   Time    `json:"updated_at"`
}

func newIncident(in store.Incident) Incident {
	return Incident{
		ID:          in.ID,
		Title:       in.Title,
		Description: in.Description,
		Service:     in.Service,
		CreatedAt:   NewTime(in.CreatedAt),
		UpdatedAt:   NewTime(in.UpdatedAt),
	}
}

// newIncidentList converts a page of stored incidents, always producing a
// non-nil slice so the JSON is [] rather than null.
func newIncidentList(page store.Page[store.Incident]) list[Incident] {
	items := make([]Incident, 0, len(page.Items))
	for _, in := range page.Items {
		items = append(items, newIncident(in))
	}
	return list[Incident]{Items: items, NextCursor: page.NextCursor}
}

// Document is the wire shape of an uploaded document.
//
// Three columns are deliberately absent. storage_path is a server filesystem
// detail that would tell a browser about the container's layout and would
// invite a client to construct one; attempts and processing_started_at are
// ingestion bookkeeping that means nothing outside the pipeline.
// content_sha256 is returned, because a client that just uploaded a file can
// use it to confirm what arrived.
type Document struct {
	ID            string  `json:"id"`
	Filename      string  `json:"filename"`
	Format        string  `json:"format"`
	SizeBytes     int64   `json:"size_bytes"`
	ContentSHA256 string  `json:"content_sha256"`
	Service       *string `json:"service"`
	DocumentType  string  `json:"document_type"`
	Status        string  `json:"status"`
	FailureReason *string `json:"failure_reason"`
	ChunkCount    int     `json:"chunk_count"`
	CreatedAt     Time    `json:"created_at"`
	UpdatedAt     Time    `json:"updated_at"`
}

func newDocument(d store.Document) Document {
	return Document{
		ID:            d.ID,
		Filename:      d.Filename,
		Format:        d.Format,
		SizeBytes:     d.SizeBytes,
		ContentSHA256: d.ContentSHA256,
		Service:       d.Service,
		DocumentType:  d.DocumentType,
		Status:        d.Status,
		FailureReason: d.FailureReason,
		ChunkCount:    d.ChunkCount,
		CreatedAt:     NewTime(d.CreatedAt),
		UpdatedAt:     NewTime(d.UpdatedAt),
	}
}

func newDocumentList(page store.Page[store.Document]) list[Document] {
	items := make([]Document, 0, len(page.Items))
	for _, d := range page.Items {
		items = append(items, newDocument(d))
	}
	return list[Document]{Items: items, NextCursor: page.NextCursor}
}
