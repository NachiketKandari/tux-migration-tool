package llm

import "strings"

// ExtractFenced returns the first fenced code block tagged lang ("go",
// "python", …) from a model response, trimmed of newline padding only — a
// code body's leading/trailing blank lines are noise, but any other
// indentation is meaningful and must survive. Without a lang-tagged fence it
// falls back to the first bare fence, else the raw text. This is the one
// response-extraction contract for every LLM seam.
func ExtractFenced(content, lang string) string {
	tag := "```" + lang
	trimNL := func(s string) string { return strings.Trim(s, "\r\n") }
	if i := strings.Index(content, tag); i >= 0 {
		rest := content[i+len(tag):]
		if j := strings.Index(rest, "```"); j >= 0 {
			return trimNL(rest[:j])
		}
		return trimNL(rest)
	}
	if i := strings.Index(content, "```"); i >= 0 {
		rest := content[i+len("```"):]
		if j := strings.Index(rest, "```"); j >= 0 {
			return trimNL(rest[:j])
		}
	}
	return trimNL(content)
}

// JSONObject returns the outermost JSON object of a model response
// (fence- and prose-tolerant: the first '{' through the last '}'), or ""
// when none is present.
func JSONObject(content string) string {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return ""
	}
	return content[start : end+1]
}
