package k8s

import "bytes"

// SplitYAMLDocs splits a YAML stream on the "---" document separator.
// Conservative ("\n---\n" only, plus a leading "---\n") — matches the way
// kubectl/yaml.NewYAMLOrJSONDecoder splits streams in practice.
func SplitYAMLDocs(data []byte) [][]byte {
	parts := bytes.Split(data, []byte("\n---\n"))
	if len(parts) > 0 && bytes.HasPrefix(parts[0], []byte("---\n")) {
		parts[0] = parts[0][4:]
	}
	return parts
}
