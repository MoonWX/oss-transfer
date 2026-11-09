import { api, ApiError } from "./api";

/** Messages are read when they are shown, so they follow the current language. */
export type TicketDownloadMessages = {
  /** The ticket reply did not contain a ticket. */
  invalidTicket: () => string;
  /** The hidden frame reported a load error. */
  failed: () => string;
  /** The server answered with a page instead of an attachment. */
  noAttachment: () => string;
  /** The frame's document could not be inspected. */
  unreadable: () => string;
};

export type TicketDownload = {
  /** Authenticated GET that returns {data: {ticket}}. */
  ticketPath: string;
  /** POST target that takes the ticket as the form field "ticket". */
  downloadPath: string;
  /** Prefix of the hidden frame's name. */
  frameName: string;
  /** Accessible title of the hidden frame. */
  frameTitle: string;
  /** data-* attributes that identify the frame. */
  dataset?: Record<string, string>;
  messages: TicketDownloadMessages;
  /** Reports a failure that arrives after the form was submitted. */
  onError: (message: string) => void;
};

let downloadSequence = 0;

/**
 * Downloads an attachment with the browser's own download manager. A bearer
 * request obtains a short-lived, single-use ticket; a native form POST in a
 * hidden frame then exchanges it for the file, so large files stream to disk
 * instead of into memory and the URL never carries the ticket. Resolves once
 * the form is submitted; a JSON or HTML error page that loads in the frame
 * is passed to onError instead.
 */
export async function ticketDownload(options: TicketDownload): Promise<void> {
  const { messages } = options;
  const response = await api<{ data?: { ticket?: string } }>(
    options.ticketPath,
  );
  const ticket = response.data?.ticket;
  if (typeof ticket !== "string" || ticket.trim() === "")
    throw new ApiError(messages.invalidTicket());

  const frame = document.createElement("iframe");
  frame.name = `${options.frameName}-${Date.now()}-${++downloadSequence}`;
  frame.title = options.frameTitle;
  frame.hidden = true;
  frame.tabIndex = -1;
  frame.setAttribute("aria-hidden", "true");
  frame.setAttribute(
    "sandbox",
    "allow-forms allow-same-origin allow-downloads",
  );
  for (const [key, value] of Object.entries(options.dataset ?? {}))
    frame.dataset[key] = value;
  frame.referrerPolicy = "no-referrer";

  const fail = (message: string) => {
    frame.remove();
    options.onError(message);
  };
  frame.addEventListener("error", () => fail(messages.failed()));
  frame.addEventListener("load", () => {
    try {
      const doc = frame.contentDocument;
      if (!doc) {
        fail(messages.failed());
        return;
      }
      if (doc.URL === "about:blank") return;
      const text = doc.body?.textContent || "";
      let message = messages.noAttachment();
      try {
        const error = JSON.parse(text);
        if (typeof error.message === "string" && error.message.trim())
          message = error.message;
      } catch {
        // An HTML or empty response is not an attachment.
      }
      fail(message);
    } catch {
      fail(messages.unreadable());
    }
  });

  const form = document.createElement("form");
  form.method = "POST";
  form.action = options.downloadPath;
  form.target = frame.name;
  form.hidden = true;
  const field = document.createElement("input");
  field.type = "hidden";
  field.name = "ticket";
  field.value = ticket;
  form.append(field);
  document.body.append(frame, form);
  try {
    form.submit();
  } catch (error) {
    frame.remove();
    throw error;
  } finally {
    form.remove();
  }
  // Attachments do not fire load. Keep each download target until document
  // unload so navigating the app or starting another download cannot cancel it.
}
