package execution

import (
	"errors"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// stagingNames match the hidden staging directory and temporary file names
// in local file errors, whose 64- and 32-digit identifiers push the cause
// off narrow screens.
var stagingNames = regexp.MustCompile(`\.oss-transfer-(staging-)?[0-9a-f]{64}-[0-9a-f]{32}(\.tmp)?`)

// objectFailureStage matches the stage and quoted key an object failure
// names (see Engine.copyItem). A file-system cause may precede them.
var objectFailureStage = regexp.MustCompile(`(?:^|: )(?:open source|read source|reconcile source|reconcile legacy result|check destination|write destination|verify write) "(?:[^"\\]|\\.)*": `)

// ObjectErrorCause is the text of an object failure without its stage and
// quoted key, so that words matched in it never come from the key. Other
// text is returned as is.
func ObjectErrorCause(text string) string {
	match := objectFailureStage.FindStringIndex(text)
	if match == nil {
		return text
	}
	if match[0] == 0 {
		return text[match[1]:]
	}
	return text[:match[0]] + ": " + text[match[1]:]
}

// objectErrorText formats an object failure for operators: the cause of a
// file-system error (such as "no space left on device") comes first, and
// staging names are shortened. Other errors keep their text.
func objectErrorText(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	var cause error
	var pathError *fs.PathError
	var linkError *os.LinkError
	var syscallError *os.SyscallError
	switch {
	case errors.As(err, &pathError):
		cause = pathError.Err
	case errors.As(err, &linkError):
		cause = linkError.Err
	case errors.As(err, &syscallError):
		cause = syscallError.Err
	}
	if cause != nil {
		if prefix := cause.Error(); prefix != "" && !strings.HasPrefix(text, prefix) {
			text = prefix + ": " + text
		}
		text = stagingNames.ReplaceAllStringFunc(text, func(name string) string {
			if strings.HasPrefix(name, ".oss-transfer-staging-") {
				return ".oss-transfer-staging-…"
			}
			return ".oss-transfer-….tmp"
		})
	}
	return errorText(errors.New(text))
}
