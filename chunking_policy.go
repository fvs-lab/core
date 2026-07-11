package core

// Chunking policy 1: content-sniffed parameters. The rule is part of the
// on-disk format: it depends only on the file's leading bytes, never on its
// name, so every implementation chunks identical content identically and
// cross-repository dedup survives renames.

// TextChunkParams is the CDC target for text-like content: small chunks so a
// scattered one-line edit invalidates kilobytes, not a 16 KiB block.
func TextChunkParams() ChunkParams {
	return ChunkParams{Min: 1024, Avg: 4096, Max: 16384}
}

// sniffWindow is how many leading bytes the text heuristic examines.
const sniffWindow = 8192

// IsTextHead classifies content from its leading bytes: no NUL and at least
// 70% printable (tab, newlines, printable ASCII, and any byte >= 128 so
// UTF-8 sequences count). Carved in stone as policy 1 of the format.
func IsTextHead(head []byte) bool {
	if len(head) == 0 {
		return false
	}
	if len(head) > sniffWindow {
		head = head[:sniffWindow]
	}
	printable := 0
	for _, b := range head {
		if b == 0 {
			return false
		}
		if b == '\t' || b == '\n' || b == '\r' || (b >= 32 && b < 127) || b >= 128 {
			printable++
		}
	}
	return printable*100 >= len(head)*70
}

// ParamsForContent picks the chunking parameters under a policy: policy 0
// always uses base; policy 1 switches text-like content to TextChunkParams.
func ParamsForContent(policy int, base ChunkParams, head []byte) ChunkParams {
	if policy >= 1 && IsTextHead(head) {
		return TextChunkParams()
	}
	return base
}
