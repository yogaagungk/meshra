package model

import (
	"context"
	"io"
	"time"
)

const MaxAttachmentBytes int64 = 25 * 1024 * 1024

type Attachment struct {
	ID          string    `json:"id"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	MIME        string    `json:"mime"`
	Name        string    `json:"name"`
	Owner       string    `json:"owner"`
	CreatedAt   time.Time `json:"createdAt"`
	Unavailable bool      `json:"unavailable,omitempty"`
}

type AttachmentService interface {
	UploadAttachment(context.Context, Principal, string, string, string, io.Reader, int64) (Attachment, error)
	ReadAttachment(context.Context, Principal, string) (Attachment, io.ReadCloser, error)
}
