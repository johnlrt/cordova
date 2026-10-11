import { LiveConnection } from './connection.ts';
import type { LiveEvent } from './protocol.ts';
import { session } from '$lib/state/session.svelte.ts';

export function startNodeLive(onChange: (resourceIDs: string[] | null) => void): () => void {
  const current = session.session;
  if (!current) return () => {};

  const generation = session.generation;
  const isCurrent = () => session.generation === generation && session.session?.account.id === current.account.id;
  const pending = new Set<string>();
  let refreshAll = false;
  let flushTimer: number | undefined;
  const queueChange = (resourceID: string | null) => {
    if (resourceID === null) refreshAll = true;
    else pending.add(resourceID);
    if (flushTimer !== undefined) return;
    flushTimer = window.setTimeout(() => {
      flushTimer = undefined;
      const changed = refreshAll ? null : [...pending];
      refreshAll = false;
      pending.clear();
      onChange(changed);
    }, 500);
  };

  const connection = new LiveConnection(current, generation, {
    onStatus: () => {},
    onInvalidate: (event: LiveEvent) => {
      if (event.topic === 'nodes') queueChange(event.resource_id);
    },
    onResync: () => queueChange(null),
    onPoll: () => queueChange(null)
  }, isCurrent, () => session.expireIfCurrent(generation));
  const expiryPoll = window.setInterval(() => {
    if (document.visibilityState === 'visible' && isCurrent()) queueChange(null);
  }, 30_000);

  connection.start();
  return () => {
    window.clearInterval(expiryPoll);
    if (flushTimer !== undefined) window.clearTimeout(flushTimer);
    connection.stop();
  };
}
