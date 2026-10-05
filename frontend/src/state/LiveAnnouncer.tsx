import React, { createContext, useCallback, useContext, useMemo, useRef, useState } from "react";

export type Politeness = "polite" | "assertive";

type Announce = (message: string, politeness?: Politeness) => void;

const AnnouncerContext = createContext<Announce>(() => {});

interface Message {
  id: number;
  text: string;
}

/**
 * App-level live regions (WCAG 2.2 SC 4.1.3 Status Messages). The regions are
 * rendered once and stay in the DOM, so screen readers pick up every message
 * added later: progress and results politely, errors assertively.
 */
export const LiveAnnouncerProvider: React.FC<React.PropsWithChildren> = ({ children }) => {
  const [polite, setPolite] = useState<Message | null>(null);
  const [assertive, setAssertive] = useState<Message | null>(null);
  const nextId = useRef(0);

  const announce = useCallback<Announce>((message, politeness = "polite") => {
    const text = message.trim();
    if (!text) return;
    // A fresh key re-inserts the node, so repeating the same text is announced again.
    const entry = { id: ++nextId.current, text };
    if (politeness === "assertive") setAssertive(entry);
    else setPolite(entry);
  }, []);

  const value = useMemo(() => announce, [announce]);

  return (
    <AnnouncerContext.Provider value={value}>
      {children}
      <div className="pf-v6-screen-reader" role="status" aria-live="polite" aria-atomic="true" data-testid="live-region-polite">
        {polite && <span key={polite.id}>{polite.text}</span>}
      </div>
      <div className="pf-v6-screen-reader" role="alert" aria-live="assertive" aria-atomic="true" data-testid="live-region-assertive">
        {assertive && <span key={assertive.id}>{assertive.text}</span>}
      </div>
    </AnnouncerContext.Provider>
  );
};

export function useAnnounce(): Announce {
  return useContext(AnnouncerContext);
}
