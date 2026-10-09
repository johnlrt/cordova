import type { LiveEvent } from './protocol.ts';

export class InvalidationDeduper {
  private eventIds = new Set<string>();
  private revisions = new Map<string, number>();
  private insertionOrder: string[] = [];

  accept(event: LiveEvent): boolean {
    const key = `${event.topic}:${event.resource_id}`;
    if (this.eventIds.has(event.event_id) || (this.revisions.get(key) ?? 0) >= event.revision) return false;
    this.eventIds.add(event.event_id);
    this.insertionOrder.push(event.event_id);
    if (this.insertionOrder.length > 2048) this.eventIds.delete(this.insertionOrder.shift()!);
    this.revisions.set(key, event.revision);
    if (this.revisions.size > 2048) this.revisions.delete(this.revisions.keys().next().value!);
    return true;
  }

  reset() { this.eventIds.clear(); this.revisions.clear(); this.insertionOrder = []; }
}
