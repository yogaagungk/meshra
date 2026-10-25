package model

// SkillReference pins reviewed instruction content for an admitted task. A
// later library edit or binding update cannot silently replace this version.
type SkillReference struct {
 ID string `json:"id"`
 Version int `json:"version"`
 SHA256 string `json:"sha256"`
}
