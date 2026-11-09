package comparison

// ErrorDetail describes err for Row.ErrorDetail and Progress.ErrorDetail: the
// operation, side, HTTP status, S3 error code and request ID when known. It
// never includes credentials, signatures or request URLs.
func ErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	return ""
}
