import { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { useStableMessages } from "../src/lib/stableMessages";

declare global {
  interface Window {
    /** Delivers one refresh: a new snapshot carrying these messages. */
    pushSnapshot: (messages: string[]) => void;
  }
}

// Nothing re-renders this component except a pushed snapshot and the hook's
// own timer, so the tests see exactly what the hook schedules.
function StableMessagesFixture() {
  const refreshMs = Number(
    new URLSearchParams(location.search).get("refreshMs") || 0,
  );
  const [snapshot, setSnapshot] = useState({ messages: [] as string[] });
  useEffect(() => {
    window.pushSnapshot = (messages) => setSnapshot({ messages });
  }, []);
  const shown = useStableMessages(snapshot.messages, snapshot, { refreshMs });
  return (
    <ul data-testid="shown">
      {shown.map((message) => (
        <li key={message}>{message}</li>
      ))}
    </ul>
  );
}

createRoot(document.getElementById("root")!).render(<StableMessagesFixture />);
