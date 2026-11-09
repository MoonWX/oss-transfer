/**
 * Automatic addressing, as the Agent resolves it (transferstore.AutoPathStyle):
 * path-style when the endpoint has an explicit port, or its host is an IP
 * address, "localhost", a name without a dot, or ends with .internal, .local
 * or .lan; otherwise virtual-hosted. Returns undefined for an endpoint that
 * is not a URL.
 */
export function autoPathStyle(endpoint: string): boolean | undefined {
  const value = endpoint.trim();
  try {
    new URL(value);
  } catch {
    return undefined;
  }
  // Classify the host as written, as Go's url.Parse keeps it: URL would
  // rewrite http://10.1 or http://1.2.3.04 to a dotted-quad address, which
  // Go's net.ParseIP does not accept, and drop a port that is the scheme's
  // default, which the Agent still sees.
  const authority = /^[a-z][a-z\d+.-]*:\/\/([^/?#]*)/i.exec(value)?.[1];
  if (authority === undefined) return undefined;
  const hostPort = authority.slice(authority.lastIndexOf("@") + 1);
  if (hostPort.startsWith("[")) return true;
  const port = /:(\d*)$/.exec(hostPort);
  if (port?.[1]) return true;
  const host = hostPort
    .slice(0, port ? port.index : undefined)
    .toLowerCase()
    .replace(/\.$/, "");
  if (!host) return undefined;
  if (host === "localhost" || ipv4(host) || !host.includes(".")) return true;
  return [".internal", ".local", ".lan"].some((suffix) =>
    host.endsWith(suffix),
  );
}

/** A dotted-quad IPv4 address as Go's net.ParseIP accepts it: no leading zeros. */
function ipv4(host: string) {
  const parts = host.split(".");
  return (
    parts.length === 4 &&
    parts.every((part) => /^(?:0|[1-9]\d{0,2})$/.test(part) && +part <= 255)
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
