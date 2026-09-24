import { useEffect, useState } from "react";
import { Badge, Card, formatClock, Spinner } from "../../ds/index.ts";
import { errorText, streamSSE, useSession, type FeedEvent } from "../../api/index.ts";
import { useScope } from "../scope.tsx";
import { ErrorStrip, RunLink } from "./common.tsx";
import { eventSummary } from "./events.ts";

const MAX = 50;

/** Live feed from GET /v1/events (SSE), newest first. */
export function ActivityFeed() {
  const scope = useScope();
  const session = useSession();
  const [events, setEvents] = useState<FeedEvent[]>([]);
  const [status, setStatus] = useState<"connecting" | "live" | "error">("connecting");
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const ctrl = new AbortController();
    setEvents([]);
    setStatus("connecting");
    void streamSSE("/events", {
      query: { follow: true, last: MAX },
      tenant: scope.apiTenant,
      signal: ctrl.signal,
      onOpen: () => {
        setStatus("live");
        setError(null);
      },
      onMessage: (m) => {
        if (m.event !== "lux") return;
        try {
          const e = JSON.parse(m.data) as FeedEvent;
          setEvents((xs) => [e, ...xs].slice(0, MAX));
        } catch {}
      },
      onClose: (_reason, err) => {
        setStatus("error");
        // A clean end (no error) keeps the last error visible until a healthy reconnect.
        if (err) setError(errorText(err));
      },
    });
    return () => ctrl.abort();
  }, [scope.apiTenant]);

  const showTenant = session.role === "operator" && scope.apiTenant === undefined;
  return (
    <Card
      title="Activity"
      subtitle={status === "live" ? "live" : status === "connecting" ? "connecting…" : "reconnecting…"}
      actions={status === "live" ? <span className="pill pill-teal pill-live pill-compact"><span className="pill-dot" /></span> : <Spinner size={12} />}
      flush
      className="feed-card"
    >
      <ErrorStrip error={status === "live" ? null : error} />
      {events.length === 0 ? (
        <div className="feed-empty muted">Waiting for events. New run activity appears here as it happens.</div>
      ) : (
        <ul className="feed">
          {events.map((e) => (
            <li key={e.id} className="feed-item">
              <span className="feed-time mono">{formatClock(e.time)}</span>
              <span className="feed-body">
                <span className="feed-line">
                  <Badge mono outline>
                    {e.type}
                  </Badge>
                  <span className="feed-text">{eventSummary(e)}</span>
                </span>
                <span className="feed-meta">
                  <RunLink id={e.runId} truncate={14} />
                  {showTenant && <span className="muted">{e.tenant}</span>}
                  {e.epoch != null && e.epoch > 0 && <span className="muted mono">e{e.epoch}</span>}
                </span>
              </span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}
