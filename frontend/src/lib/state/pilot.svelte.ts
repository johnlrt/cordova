import { ApiFailure, decodeHealth, decodeModels, requestJSON, type Health, type Model } from '../api/client';

export interface Sample { time: number; latency: number; online: boolean }

export class Pilot {
  health = $state<Health | null>(null);
  status = $state<'checking' | 'online' | 'offline'>('checking');
  latency = $state<number | null>(null);
  samples = $state<Sample[]>([]);
  models = $state<Model[]>([]);
  catalog = $state<'disconnected' | 'loading' | 'ready' | 'error'>('disconnected');
  catalogError = $state<{ code: string; status: number } | null>(null);
  connected = $state(false);
  refreshing = $state(false);
  private key = '';
  private healthController?: AbortController;
  private modelController?: AbortController;
  private generation = 0;

  async refreshHealth() {
    if (this.refreshing) return;
    this.refreshing = true;
    const controller = this.healthController = new AbortController();
    const start = performance.now();
    try {
      const health = decodeHealth(await requestJSON('/healthz', controller.signal));
      if (controller.signal.aborted) return;
      this.health = health;
      this.status = 'online';
      this.latency = Math.round(performance.now() - start);
      this.samples = [...this.samples, { time: Date.now(), latency: this.latency, online: true }].slice(-12);
    } catch {
      if (controller.signal.aborted) return;
      this.status = 'offline';
      this.latency = null;
      this.samples = [...this.samples, { time: Date.now(), latency: 0, online: false }].slice(-12);
    } finally { this.refreshing = false; }
  }

  async connect(key: string) {
    this.disconnect();
    this.key = key.trim();
    await this.refreshCatalog();
  }

  async refreshCatalog() {
    if (!this.key) return;
    this.modelController?.abort();
    const controller = this.modelController = new AbortController();
    const generation = ++this.generation;
    this.catalog = 'loading';
    this.catalogError = null;
    try {
      const models = decodeModels(await requestJSON('/v1/models', controller.signal, this.key));
      if (controller.signal.aborted || generation !== this.generation) return;
      this.models = models;
      this.connected = true;
      this.catalog = 'ready';
    } catch (error) {
      if (controller.signal.aborted || generation !== this.generation) return;
      this.models = [];
      this.connected = false;
      this.key = '';
      this.catalog = 'error';
      this.catalogError = error instanceof ApiFailure ? { code: error.code, status: error.status } : { code: 'unavailable', status: 0 };
    }
  }

  disconnect() {
    this.generation++;
    this.modelController?.abort();
    this.key = '';
    this.connected = false;
    this.models = [];
    this.catalog = 'disconnected';
    this.catalogError = null;
  }

  dispose() { this.healthController?.abort(); this.disconnect(); }
}
