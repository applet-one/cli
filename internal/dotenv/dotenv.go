// Package dotenv parses dotenv files with the same rules as Node.js
// util.parseEnv, which the original Applet CLI used. Values are never
// interpolated, so secrets containing "$" are preserved exactly.
package dotenv

import "strings"

// Parse returns the KEY=value entries in text. Later entries win.
func Parse(text string) map[string]string {
	values := map[string]string{}
	content := strings.ReplaceAll(text, "\r", "")
	for {
		content = strings.TrimLeft(content, " \t\n")
		if content == "" {
			break
		}
		if strings.HasPrefix(content, "#") {
			content = afterLine(content)
			continue
		}
		equal := strings.IndexByte(content, '=')
		if equal < 0 {
			break
		}
		if newline := strings.IndexByte(content, '\n'); newline >= 0 && newline < equal {
			content = content[newline+1:]
			continue
		}
		key := strings.TrimSpace(content[:equal])
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		content = strings.TrimLeft(content[equal+1:], " \t")

		if content != "" && strings.ContainsRune("\"'`", rune(content[0])) {
			quote := content[0]
			if end := strings.IndexByte(content[1:], quote); end >= 0 {
				value := content[1 : 1+end]
				if quote == '"' {
					value = strings.ReplaceAll(value, `\n`, "\n")
				}
				if key != "" {
					values[key] = value
				}
				content = afterLine(content[2+end:])
				continue
			}
		}

		line := content
		if newline := strings.IndexByte(content, '\n'); newline >= 0 {
			line, content = content[:newline], content[newline+1:]
		} else {
			content = ""
		}
		if hash := strings.IndexByte(line, '#'); hash >= 0 {
			line = line[:hash]
		}
		if key != "" {
			values[key] = strings.TrimSpace(line)
		}
	}
	return values
}

func afterLine(s string) string {
	if newline := strings.IndexByte(s, '\n'); newline >= 0 {
		return s[newline+1:]
	}
	return ""
}
