function abortReason(signal: AbortSignal): unknown {
  // Older AbortController implementations do not expose signal.reason.
  return signal.reason === undefined
    ? new DOMException("The request was cancelled.", "AbortError")
    : signal.reason;
}

export function throwIfAborted(signal: AbortSignal): void {
  if (signal.aborted) throw abortReason(signal);
}

// Each request owns its timer and parent listener until the response body has
// been consumed. No AbortSignal static methods or global patches are needed.
export function createRequestAbort(
  parent: AbortSignal | undefined,
  timeoutMs: number,
) {
  if (
    !Number.isSafeInteger(timeoutMs) ||
    timeoutMs < 0 ||
    timeoutMs > 2_147_483_647
  )
    throw new RangeError("Invalid request timeout");
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  let reason: unknown;
  let disposed = false;
  function dispose() {
    if (disposed) return;
    disposed = true;
    if (timer !== undefined) clearTimeout(timer);
    parent?.removeEventListener("abort", onParentAbort);
  }
  function abort(cause: unknown) {
    if (disposed || controller.signal.aborted) return;
    reason = cause;
    controller.abort(cause);
    dispose();
  }
  function onParentAbort() {
    abort(abortReason(parent!));
  }
  if (parent?.aborted) onParentAbort();
  else {
    parent?.addEventListener("abort", onParentAbort, { once: true });
    timer = setTimeout(
      () => abort(new DOMException("The request timed out.", "TimeoutError")),
      timeoutMs,
    );
  }
  return {
    signal: controller.signal,
    get reason() {
      return reason;
    },
    dispose,
  };
}
