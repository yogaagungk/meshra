package relay

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"meshra/internal/model"
)

const templateMaxBytes = 25 * 1024 * 1024
const templateMaxEntries = 100

type skillAsset struct {
	Path   string `json:"path"`
	MIME   string `json:"mime"`
	Data   []byte `json:"data"`
	SHA256 string `json:"sha256"`
}

type templateArchiveEntry struct {
	name      string
	data      []byte
	directory bool
}

func templateDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func templateArchivePath(name string, directory bool) (string, error) {
	if name == "" || len(name) > 4096 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00:") || strings.HasPrefix(name, "/") {
		return "", model.Err(400, "unsafe archive path")
	}
	if directory {
		name = strings.TrimSuffix(name, "/")
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", model.Err(400, "unsafe archive path")
		}
	}
	if path.Clean(name) != name {
		return "", model.Err(400, "unsafe archive path")
	}
	return name, nil
}

// No archive entry is extracted onto the host. Both compressed input and decoded
// bytes are bounded; metadata-only directories count against the entry limit.
func readTemplateArchive(data []byte) ([]templateArchiveEntry, error) {
	if len(data) > templateMaxBytes {
		return nil, model.Err(413, "archive exceeds 25 MiB")
	}
	entries := []templateArchiveEntry{}
	seen := map[string]bool{}
	total := int64(0)
	add := func(name string, directory bool, size int64, reader io.Reader) error {
		if len(entries) >= templateMaxEntries {
			return model.Err(413, "archive exceeds 100 entries")
		}
		clean, err := templateArchivePath(name, directory)
		if err != nil {
			return err
		}
		if _, exists := seen[clean]; exists {
			return model.Err(400, "duplicate archive entry")
		}
		for parent := path.Dir(clean); parent != "."; parent = path.Dir(parent) {
			if isDir, exists := seen[parent]; exists && !isDir {
				return model.Err(400, "archive file shadows directory")
			}
		}
		if !directory {
			for prior := range seen {
				if strings.HasPrefix(prior, clean+"/") {
					return model.Err(400, "archive file shadows directory")
				}
			}
		}
		if size < 0 || size > templateMaxBytes-total {
			return model.Err(413, "archive exceeds 25 MiB")
		}
		var content []byte
		if !directory {
			content, err = io.ReadAll(io.LimitReader(reader, size+1))
			if err != nil {
				return model.Err(400, "invalid archive content")
			}
			if int64(len(content)) != size {
				return model.Err(400, "archive entry size mismatch")
			}
			total += size
		} else if size != 0 {
			return model.Err(400, "archive directory has content")
		}
		seen[clean] = directory
		entries = append(entries, templateArchiveEntry{clean, content, directory})
		return nil
	}
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{'P', 'K', 3, 4}) {
		archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, model.Err(400, "invalid ZIP archive")
		}
		if len(archive.File) > templateMaxEntries {
			return nil, model.Err(413, "archive exceeds 100 entries")
		}
		for _, file := range archive.File {
			mode := file.Mode()
			if !mode.IsRegular() && !mode.IsDir() {
				return nil, model.Err(400, "archive links and special files are forbidden")
			}
			if file.UncompressedSize64 > templateMaxBytes {
				return nil, model.Err(413, "archive entry exceeds 25 MiB")
			}
			body, err := file.Open()
			if err != nil {
				return nil, model.Err(400, "invalid ZIP entry")
			}
			err = add(file.Name, mode.IsDir(), int64(file.UncompressedSize64), body)
			closeErr := body.Close()
			if err != nil {
				return nil, err
			}
			if closeErr != nil {
				return nil, model.Err(400, "invalid ZIP entry")
			}
		}
	} else {
		var reader io.Reader = bytes.NewReader(data)
		if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
			compressed, err := gzip.NewReader(reader)
			if err != nil {
				return nil, model.Err(400, "invalid gzip archive")
			}
			defer compressed.Close()
			reader = compressed
		}
		// Account for TAR headers, padding and PAX metadata as well as file bytes.
		limited := &io.LimitedReader{R: reader, N: templateMaxBytes + templateMaxEntries*4096 + 1}
		archive := tar.NewReader(limited)
		for {
			header, err := archive.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, model.Err(400, "invalid TAR archive")
			}
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA && header.Typeflag != tar.TypeDir {
				return nil, model.Err(400, "archive links and special files are forbidden")
			}
			if header.Linkname != "" || header.PAXRecords["GNU.sparse.map"] != "" || len(header.PAXRecords) > 0 {
				return nil, model.Err(400, "archive extended metadata is forbidden")
			}
			if err = add(header.Name, header.Typeflag == tar.TypeDir, header.Size, archive); err != nil {
				return nil, err
			}
		}
		// Drain only zero padding, validating gzip checksums too. A second hidden
		// archive after the end marker must not evade entry and duplicate checks.
		var padding [4096]byte
		for {
			n, err := limited.Read(padding[:])
			for _, b := range padding[:n] {
				if b != 0 {
					return nil, model.Err(400, "archive has trailing non-padding content")
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, model.Err(400, "invalid compressed archive")
			}
		}
		if limited.N <= 0 {
			return nil, model.Err(413, "archive metadata exceeds limit")
		}
	}
	if len(entries) == 0 {
		return nil, model.Err(400, "empty archive")
	}
	return entries, nil
}

// Skills export in the portable folder form, avoiding base64 expansion beyond
// the archive's decoded-byte limit. Other definitions use one inert JSON file.
func encodeTemplateArchive(value localTemplate) ([]byte, error) {
	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	write := func(name string, data []byte) error {
		file, err := archive.Create(name)
		if err != nil {
			return err
		}
		_, err = file.Write(data)
		return err
	}
	if value.Kind == "skill" {
		var skill localSkillVersion
		if err := json.Unmarshal(value.Definition, &skill); err != nil {
			return nil, err
		}
		if err := write("SKILL.md", []byte(skill.Markdown)); err != nil {
			return nil, err
		}
		for _, asset := range skill.Assets {
			if err := write(asset.Path, asset.Data); err != nil {
				return nil, err
			}
		}
	} else {
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if err = write("template.json", body); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if output.Len() > templateMaxBytes {
		return nil, model.Err(413, "export archive exceeds 25 MiB")
	}
	return output.Bytes(), nil
}

func skillFromArchive(entries []templateArchiveEntry, name, description string) (localSkillVersion, error) {
	var skill localSkillVersion
	root := ""
	found := false
	for _, entry := range entries {
		if !entry.directory && path.Base(entry.name) == "SKILL.md" {
			if found {
				return skill, model.Err(400, "archive must contain exactly one SKILL.md")
			}
			found = true
			root = path.Dir(entry.name)
			skill.Markdown = string(entry.data)
		}
	}
	if !found {
		return skill, model.Err(400, "archive requires SKILL.md")
	}
	if err := validateReviewedSkill(name, description, skill.Markdown); err != nil {
		return skill, err
	}
	skill.Name = name
	skill.Description = description
	skill.Markdown = redact(skill.Markdown)
	for _, entry := range entries {
		if entry.directory {
			continue
		}
		relative := entry.name
		if root != "." {
			if !strings.HasPrefix(relative, root+"/") {
				return skill, model.Err(400, "archive files must be inside the SKILL.md folder")
			}
			relative = strings.TrimPrefix(relative, root+"/")
		}
		if relative == "SKILL.md" {
			continue
		}
		mediaType := mime.TypeByExtension(path.Ext(relative))
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		skill.Assets = append(skill.Assets, skillAsset{Path: relative, MIME: mediaType, Data: entry.data, SHA256: templateDigest(entry.data)})
	}
	sort.Slice(skill.Assets, func(i, j int) bool { return skill.Assets[i].Path < skill.Assets[j].Path })
	return skill, nil
}

func validateReviewedSkill(name, description, markdown string) error {
	if strings.TrimSpace(name) == "" || len(name) > 128 || !safeRegistryText(name) || redact(name) != name || len(description) > 4096 || !safeRegistryText(description) || redact(description) != description || !utf8.ValidString(markdown) {
		return model.Err(400, "reviewed skill name, description and UTF-8 markdown are required")
	}
	if len(markdown) > 65536 {
		return model.Err(413, "skill markdown exceeds 64 KiB")
	}
	return nil
}
