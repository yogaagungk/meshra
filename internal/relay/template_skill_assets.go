package relay

import (
	"encoding/base64"
	"io"

	"meshra/internal/model"
)

type pinnedSkillAssetChunk struct {
	Path     string `json:"path"`
	MIME     string `json:"mime"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Offset   int64  `json:"offset"`
	Data     string `json:"data"`
	Encoding string `json:"encoding"`
	HasMore  bool   `json:"hasMore"`
}

type pinnedSkillRead struct {
	ID          string                 `json:"id"`
	Version     int                    `json:"version"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Markdown    string                 `json:"markdown"`
	SHA256      string                 `json:"sha256"`
	Assets      []templateAssetSummary `json:"assets"`
}

// Caller holds relay ownership and supplies its trusted active-turn snapshot.
// The manifest omits bytes so a 25 MiB skill never becomes a process-frame blob.
func (r *Relay) readPinnedSkillForTool(snapshot model.ProfileTaskSnapshot, id string) (pinnedSkillRead, error) {
	value, err := r.readPinnedSkill(snapshot, id)
	if err != nil {
		return pinnedSkillRead{}, err
	}
	result := pinnedSkillRead{ID: value.ID, Version: value.Version, Name: value.Name, Description: value.Description, Markdown: value.Markdown, SHA256: value.SHA256, Assets: []templateAssetSummary{}}
	for _, asset := range value.Assets {
		result.Assets = append(result.Assets, templateAssetSummary{asset.Path, asset.MIME, len(asset.Data), asset.SHA256})
	}
	for _, attachmentID := range value.AssetAttachmentIDs {
		asset, err := r.attachment(attachmentID)
		if err != nil {
			return result, err
		}
		if asset.Unavailable {
			return result, model.Err(404, "pinned skill asset is unavailable")
		}
		result.Assets = append(result.Assets, templateAssetSummary{skillAttachmentPath(asset), asset.MIME, int(asset.Size), asset.SHA256})
	}
	return result, nil
}

func skillAttachmentPath(asset model.Attachment) string { return "attachments/" + asset.ID }

// This is not a general attachment reader. A path must belong to the exact
// immutable skill version in the active task snapshot and its still-live binding.
func (r *Relay) readPinnedSkillAsset(snapshot model.ProfileTaskSnapshot, id, assetPath string, offset int64, limit int) (pinnedSkillAssetChunk, error) {
	result := pinnedSkillAssetChunk{}
	if offset < 0 || limit < 0 || limit > 64*1024 {
		return result, model.Err(400, "invalid skill asset chunk range")
	}
	if limit == 0 {
		limit = 32 * 1024
	}
	clean, err := templateArchivePath(assetPath, false)
	if err != nil {
		return result, err
	}
	value, err := r.readPinnedSkill(snapshot, id)
	if err != nil {
		return result, err
	}
	for _, asset := range value.Assets {
		if asset.Path != clean {
			continue
		}
		if templateDigest(asset.Data) != asset.SHA256 {
			return result, model.Err(409, "pinned skill asset integrity mismatch")
		}
		size := int64(len(asset.Data))
		if offset > size {
			return result, model.Err(400, "skill asset offset exceeds size")
		}
		end := min(size, offset+int64(limit))
		return pinnedSkillAssetChunk{Path: clean, MIME: asset.MIME, SHA256: asset.SHA256, Size: size, Offset: offset, Data: base64.StdEncoding.EncodeToString(asset.Data[offset:end]), Encoding: "base64", HasMore: end < size}, nil
	}
	for _, attachmentID := range value.AssetAttachmentIDs {
		asset, err := r.attachment(attachmentID)
		if err != nil {
			return result, err
		}
		if skillAttachmentPath(asset) != clean {
			continue
		}
		if offset > asset.Size {
			return result, model.Err(400, "skill asset offset exceeds size")
		}
		file, err := r.openAttachment(asset)
		if err != nil {
			return result, err
		}
		defer file.Close()
		if _, err = file.Seek(offset, io.SeekStart); err != nil {
			return result, err
		}
		data, err := io.ReadAll(io.LimitReader(file, int64(limit)))
		if err != nil {
			return result, err
		}
		return pinnedSkillAssetChunk{Path: clean, MIME: asset.MIME, SHA256: asset.SHA256, Size: asset.Size, Offset: offset, Data: base64.StdEncoding.EncodeToString(data), Encoding: "base64", HasMore: offset+int64(len(data)) < asset.Size}, nil
	}
	return result, model.Err(403, "asset is not pinned to this skill and task")
}
