import { ApiFailure } from '$lib/api/client.ts';
import { fetchRecentUsage, type UsagePage } from '$lib/api/usage.ts';
import { session } from './session.svelte.ts';

class AccountResources {
  usage = $state<UsagePage | null>(null);
  usageStatus = $state<'idle' | 'loading' | 'ready' | 'error'>('idle');
  usageError = $state<ApiFailure | null>(null);
  stale = $state(false);
  private usageController?: AbortController;
  private loadedForAccount = '';

  async refreshUsage() {
    const current = session.session;
    if (!current) return;
    const accountId = current.account.id;
    const generation = session.generation;
    this.usageController?.abort();
    const controller = this.usageController = new AbortController();
    this.usageStatus = this.usage ? 'ready' : 'loading';
    this.usageError = null;
    try {
      const usage = await fetchRecentUsage(controller.signal);
      if (controller.signal.aborted || generation !== session.generation || session.session?.account.id !== accountId) return;
      this.usage = usage;
      this.loadedForAccount = accountId;
      this.usageStatus = 'ready';
      this.stale = false;
    } catch (error) {
      if (controller.signal.aborted || generation !== session.generation) return;
      const failure = error instanceof ApiFailure ? error : new ApiFailure('unavailable');
      if (failure.status === 401) {
        session.expireIfCurrent(generation);
        return;
      }
      this.usageError = failure;
      this.usageStatus = 'error';
      this.stale = this.usage !== null;
    } finally {
      if (this.usageController === controller) this.usageController = undefined;
    }
  }

  markStale(topic: 'nodes' | 'usage' | 'billing') {
    if (topic === 'usage' && this.loadedForAccount === session.session?.account.id) this.stale = true;
  }

  clear() {
    this.usageController?.abort();
    this.usageController = undefined;
    this.usage = null;
    this.usageStatus = 'idle';
    this.usageError = null;
    this.stale = false;
    this.loadedForAccount = '';
  }
}

export const resources = new AccountResources();
