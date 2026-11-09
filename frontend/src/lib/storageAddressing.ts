/**
 * Automatic addressing, as the Agent resolves it (transferstore.AutoPathStyle):
 * path-style when the endpoint has an explicit port, or its host is an IP
 * address, "localhost", a name without a dot, or ends with .internal, .local
 * or .lan; otherwise virtual-hosted. Returns undefined for an endpoint that
 * is not a URL.
 */
export function autoPathStyle(endpoint: string): boolean | undefined {
  let url: URL;
  try {
    url = new URL(endpoint.trim());
  } catch {
    return undefined;
  }
  // URL drops a port that is the scheme's default; the Agent still sees it.
  if (url.port || /^[a-z]+:\/\/[^/?#]*:\d+(?:[/?#]|$)/i.test(endpoint.trim()))
    return true;
  const host = url.hostname.toLowerCase().replace(/\.$/, "");
  if (!host) return undefined;
  if (
    host === "localhost" ||
    host.startsWith("[") ||
    /^\d{1,3}(?:\.\d{1,3}){3}$/.test(host) ||
    !host.includes(".")
  )
    return true;
  return [".internal", ".local", ".lan"].some((suffix) =>
    host.endsWith(suffix),
  );
}

/**
 * Whether a value can be saved as an endpoint: an http or https origin
 * without credentials, path, query or fragment, like the Master requires.
 */
export function validEndpoint(endpoint: string) {
  const value = endpoint.trim();
  if (!/^https?:\/\/[^/?#\s]+\/?$/i.test(value)) return false;
  try {
    const url = new URL(value);
    return (
      (url.protocol === "http:" || url.protocol === "https:") &&
      url.hostname !== "" &&
      !url.username &&
      !url.password
    );
  } catch {
    return false;
  }
}
