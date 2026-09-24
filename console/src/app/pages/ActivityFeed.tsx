import { Badge, Card, formatClock, LiveDot, Spinner } from "../../ds/index.ts";
import { liveLabel, useLiveState } from "../../api/index.ts";
import { useScope } from "../scope.tsx";
import { ErrorStrip, RunLink } from "./common.tsx";
import { eventSummary } from "./events.ts";

/** The newest Run events from the tab's live stream, newest first. */
export function ActivityFeed() {
  const scope = useScope();
  const { recent: events, status, error } = useLiveState();

  return (
    <Card
      title="Activity"
      subtitle={liveLabel(status)}
      actions={status === "live" ? <LiveDot /> : <Spinner size={12} />}
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
                  {scope.showTenant && <span className="muted">{e.tenant}</span>}
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
