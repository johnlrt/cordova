import { ApiFailure } from '../api/client.ts';
import { matchesLiveClientFrame } from '../api/generated/contracts.ts';
import type { LiveClientFrame } from '../api/generated/contracts.ts';
import { fetchLiveTicket, type SessionView } from '../api/account.ts';
import { InvalidationDeduper } from './invalidation.ts';
import { decodeLiveFrame, isFirstReadyFrame, isNextSequence, liveEvent, type LiveEvent } from './protocol.ts';

export type LiveStatus = 'connecting' | 'connected' | 'disconnected' | 'hidden';
export interface LiveCallbacks {
  onStatus(status: LiveStatus): void;
  onInvalidate(event: LiveEvent): void;
  onResync(): void;
  onPoll(): void;
}

export function retryDelay(attempt: number, random = Math.random): number {
  const ceiling = Math.min(60_000, 1000 * 2 ** Math.min(Math.max(attempt, 0), 6));
  return Math.floor(random() * ceiling);
}

function id() { return crypto.randomUUID(); }

export class LiveConnection {
  private readonly account: SessionView;
  private readonly generation: number;
  private readonly callbacks: LiveCallbacks;
  private readonly isSessionCurrent: () => boolean;
  private readonly onSessionExpired: () => void;
  private socket?: WebSocket;
  private status: LiveStatus = 'disconnected';
  private sessionId = '';
  private nextOutSequence = 1;
  private lastInSequence = 0;
  private authRequestId = '';
  private pendingPings = new Set<string>();
  private lastEventId: string | null = null;
  private attempt = 0;
  private runId = 0;
  private readyAt = 0;
  private lastPongAt = 0;
  private reconnectTimer?: number;
  private authTimer?: number;
  private heartbeatTimer?: number;
  private pollingTimer?: number;
  private debounce = new Map<string, number>();
  private deduper = new InvalidationDeduper();
  private onVisibility = () => {
    if (document.visibilityState === 'hidden') this.pauseForHidden();
    else if (this.status === 'hidden') { this.resetForResync(); void this.connect(this.runId); }
  };

  constructor(account: SessionView, generation: number, callbacks: LiveCallbacks,
    isSessionCurrent: () => boolean, onSessionExpired: () => void) {
    this.account = account;
    this.generation = generation;
    this.callbacks = callbacks;
    this.isSessionCurrent = isSessionCurrent;
    this.onSessionExpired = onSessionExpired;
  }

  start() {
    document.addEventListener('visibilitychange', this.onVisibility);
    if (document.visibilityState === 'hidden') this.setStatus('hidden');
    else void this.connect(this.runId);
  }

  stop() {
    this.runId++;
    document.removeEventListener('visibilitychange', this.onVisibility);
    this.clearTimers();
    for (const timer of this.debounce.values()) clearTimeout(timer);
    this.debounce.clear();
    this.socket?.close(1000, 'session ended');
    this.socket = undefined;
    this.setStatus('disconnected');
  }

  private async connect(run: number) {
    if (run !== this.runId || isTabHidden() || !this.isSessionCurrent()) return;
    this.clearConnectionTimers();
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.reconnectTimer = undefined;
    this.setStatus('connecting');
    try {
      const ticket = await fetchLiveTicket(this.account.csrf_token);
      if (run !== this.runId || isTabHidden() || !this.isSessionCurrent()) return;
      const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const socket = this.socket = new WebSocket(`${scheme}//${location.host}/api/live`);
      socket.onopen = () => {
        if (run !== this.runId || socket !== this.socket) return;
        this.nextOutSequence = 1;
        this.lastInSequence = 0;
        this.pendingPings.clear();
        this.authRequestId = id();
        this.send('live.auth', { ticket }, this.authRequestId, null);
        this.authTimer = window.setTimeout(() => socket.close(1008, 'auth timeout'), 5000);
      };
      socket.onmessage = (message) => {
        if (run !== this.runId || socket !== this.socket) return;
        if (typeof message.data !== 'string') { this.resetForResync(); socket.close(1002, 'text frames required'); return; }
        this.receive(message.data, run);
      };
      socket.onerror = () => { /* onclose owns reconnect and user-visible status. */ };
      socket.onclose = (event) => {
        if (run !== this.runId || socket !== this.socket) return;
        this.socket = undefined;
        this.clearConnectionTimers();
        if (event.code === 4001) {
          this.onSessionExpired();
          return;
        }
        this.disconnected(run);
      };
    } catch (error) {
      if (run !== this.runId) return;
      if (error instanceof ApiFailure && error.status === 401) {
        this.onSessionExpired();
        return;
      }
      this.disconnected(run, error instanceof ApiFailure && error.status === 503 ? error.retryAfter : null);
    }
  }

  private receive(raw: string, run: number) {
    let frame;
    try { frame = decodeLiveFrame(raw); }
    catch { this.resetForResync(); this.socket?.close(1002, 'invalid live frame'); return; }
    if (!isNextSequence(frame, this.lastInSequence)) { this.resetForResync(); this.socket?.close(1002, 'out of sequence'); return; }
    this.lastInSequence = frame.seq;

    if (frame.type === 'live.ready') {
      const ready = frame.payload;
      if (!isFirstReadyFrame(frame, this.authRequestId, this.account.account.id)) {
        this.resetForResync(); this.socket?.close(1008, 'session mismatch'); return;
      }
      this.sessionId = String(ready.session_id);
      this.readyAt = Date.now();
      this.lastPongAt = Date.now();
      this.attempt = 0;
      this.clearAuthTimer();
      this.send('live.subscribe', { topics: ['nodes', 'usage', 'billing'], last_event_id: this.lastEventId });
      this.setStatus('connected');
      this.stopPolling();
      this.startHeartbeat(run);
      return;
    }

    if (!this.sessionId || frame.session_id !== this.sessionId) { this.resetForResync(); this.socket?.close(1008, 'session mismatch'); return; }
    if (frame.type === 'live.event') {
      const event = liveEvent(frame);
      this.lastEventId = event.event_id;
      if (!this.deduper.accept(event)) return;
      this.scheduleInvalidation(event);
    } else if (frame.type === 'live.resync') {
      this.lastEventId = null;
      this.deduper.reset();
      for (const timer of this.debounce.values()) clearTimeout(timer);
      this.debounce.clear();
      this.callbacks.onResync();
      this.send('live.subscribe', { topics: ['nodes', 'usage', 'billing'], last_event_id: null });
    } else if (frame.type === 'live.ping') {
      if (!frame.request_id) { this.resetForResync(); this.socket?.close(1002, 'ping missing request ID'); return; }
      this.send('live.pong', frame.payload, frame.request_id);
    } else if (frame.type === 'live.pong') {
      if (!frame.request_id || !this.pendingPings.delete(frame.request_id)) {
        this.resetForResync(); this.socket?.close(1002, 'unexpected pong'); return;
      }
      this.lastPongAt = Date.now();
      if (this.readyAt && Date.now() - this.readyAt > 60_000) this.attempt = 0;
    } else if (frame.type === 'error') {
      if (frame.error?.code === 'unauthenticated' || frame.error?.code === 'stale_session') {
        this.socket?.close(4001, 'session expired');
      } else {
        this.resetForResync();
        this.socket?.close(1011, 'live service error');
      }
    }
  }

  private send(type: LiveClientFrame['type'], payload: LiveClientFrame['payload'], requestId: string | null = id(), sessionId: string | null = this.sessionId || null): string | null {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN) return null;
    const envelope = {
      v: 1, type, message_id: id(), request_id: requestId, session_id: sessionId,
      seq: this.nextOutSequence++, sent_at: new Date().toISOString(), payload, error: null
    };
    if (!matchesLiveClientFrame(envelope)) {
      this.socket.close(1002, 'invalid client frame');
      return null;
    }
    this.socket.send(JSON.stringify(envelope));
    return requestId;
  }

  private startHeartbeat(run: number) {
    this.heartbeatTimer = window.setInterval(() => {
      if (run !== this.runId || this.status !== 'connected') return;
      if (Date.now() - this.lastPongAt >= 45_000) { this.socket?.close(4000, 'heartbeat timeout'); return; }
      const requestId = this.send('live.ping', { time: new Date().toISOString() });
      if (requestId) this.pendingPings.add(requestId);
      if (this.pendingPings.size > 2) this.socket?.close(4000, 'heartbeat replies missing');
    }, 15_000);
  }

  private scheduleInvalidation(event: LiveEvent) {
    const key = `${event.topic}:${event.resource_id}`;
    const previous = this.debounce.get(key);
    if (previous) clearTimeout(previous);
    const timer = window.setTimeout(() => {
      this.debounce.delete(key);
      if (this.isSessionCurrent()) this.callbacks.onInvalidate(event);
    }, 250);
    this.debounce.set(key, timer);
  }

  private disconnected(run: number, retryAfter: number | null = null) {
    if (run !== this.runId || isTabHidden()) return;
    if (this.readyAt && Date.now() - this.readyAt > 60_000) this.attempt = 0;
    this.readyAt = 0;
    this.sessionId = '';
    this.setStatus('disconnected');
    this.startPolling();
    const delay = retryAfter === null ? retryDelay(this.attempt++) : Math.min(60_000, retryAfter * 1000);
    this.reconnectTimer = window.setTimeout(() => void this.connect(run), delay);
  }

  private startPolling() {
    if (this.pollingTimer || isTabHidden()) return;
    this.pollingTimer = window.setInterval(() => {
      if (document.visibilityState === 'visible' && this.isSessionCurrent()) this.callbacks.onPoll();
    }, 30_000);
  }

  private stopPolling() { if (this.pollingTimer) clearInterval(this.pollingTimer); this.pollingTimer = undefined; }
  private pauseForHidden() {
    this.runId++;
    this.clearTimers();
    this.socket?.close(1000, 'tab hidden');
    this.socket = undefined;
    this.setStatus('hidden');
  }
  private resetForResync() {
    this.lastEventId = null;
    this.deduper.reset();
    for (const timer of this.debounce.values()) clearTimeout(timer);
    this.debounce.clear();
    this.callbacks.onResync();
  }
  private setStatus(status: LiveStatus) { this.status = status; this.callbacks.onStatus(status); }
  private clearAuthTimer() { if (this.authTimer) clearTimeout(this.authTimer); this.authTimer = undefined; }
  private clearConnectionTimers() {
    this.clearAuthTimer();
    if (this.heartbeatTimer) clearInterval(this.heartbeatTimer);
    this.heartbeatTimer = undefined;
  }
  private clearTimers() {
    this.clearConnectionTimers();
    this.stopPolling();
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.reconnectTimer = undefined;
  }
}

function isTabHidden() { return document.visibilityState === 'hidden'; }
