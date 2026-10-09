import { ApiFailure } from '$lib/api/client.ts';
import { loadSession, loginAccount, logoutAccount, recoverAccount, registerAccount, type RegistrationResult, type SessionView } from '$lib/api/account.ts';

export type SessionStatus = 'loading' | 'signed_out' | 'signed_in' | 'error';

class SessionStore {
  status = $state<SessionStatus>('loading');
  session = $state<SessionView | null>(null);
  error = $state<ApiFailure | null>(null);
  logoutUnconfirmed = $state(false);
  generation = $state(0);
  private bootstrap?: Promise<SessionView | null>;
  private controllers = new Set<AbortController>();
  private liveCleanup = new Set<() => void>();

  async ensureLoaded(): Promise<SessionView | null> {
    if (this.status === 'signed_in') return this.session;
    if (this.status === 'signed_out') return null;
    if (this.bootstrap) return this.bootstrap;
    this.status = 'loading';
    this.error = null;
    const controller = this.controller();
    this.bootstrap = loadSession(controller.signal).then((session) => {
      if (controller.signal.aborted) return null;
      this.accept(session);
      return session;
    }).catch((error: unknown) => {
      if (controller.signal.aborted) return null;
      if (error instanceof ApiFailure && error.status === 401) {
        this.clearLocal();
        return null;
      }
      this.error = error instanceof ApiFailure ? error : new ApiFailure('unavailable');
      this.status = 'error';
      return null;
    }).finally(() => { this.release(controller); this.bootstrap = undefined; });
    return this.bootstrap;
  }

  async login(username: string, password: string, totpCode?: string): Promise<SessionView> {
    const controller = this.controller();
    try {
      const session = await loginAccount(username, password, totpCode, controller.signal);
      this.accept(session);
      return session;
    } finally { this.release(controller); }
  }

  async register(username: string, password: string): Promise<RegistrationResult> {
    const controller = this.controller();
    try {
      const result = await registerAccount(username, password, 'privacy-v1', controller.signal);
      this.accept(result);
      return result;
    } finally { this.release(controller); }
  }

  async recover(username: string, recoveryCode: string, newPassword: string): Promise<SessionView> {
    const controller = this.controller();
    try {
      const session = await recoverAccount(username, recoveryCode, newPassword, controller.signal);
      this.accept(session);
      return session;
    } finally { this.release(controller); }
  }

  addLiveCleanup(cleanup: () => void) { this.liveCleanup.add(cleanup); return () => this.liveCleanup.delete(cleanup); }

  async logout(): Promise<boolean> {
    const current = this.session;
    this.logoutUnconfirmed = false;
    this.clearLocal();
    if (!current) { this.logoutUnconfirmed = true; return false; }
    const controller = this.controller();
    try {
      await logoutAccount(current.csrf_token, controller.signal);
      return true;
    } catch (error) {
      if (error instanceof ApiFailure && error.status === 401) return true;
      this.logoutUnconfirmed = true;
      return false;
    } finally { this.release(controller); }
  }

  private accept(session: SessionView) {
    this.cancelPending();
    for (const cleanup of this.liveCleanup) cleanup();
    this.generation++;
    this.session = session;
    this.error = null;
    this.logoutUnconfirmed = false;
    this.status = 'signed_in';
  }

  private clearLocal() {
    this.cancelPending();
    for (const cleanup of this.liveCleanup) cleanup();
    this.liveCleanup.clear();
    this.generation++;
    this.session = null;
    this.error = null;
    this.status = 'signed_out';
  }

  expireIfCurrent(generation: number) {
    if (this.generation === generation) this.clearLocal();
  }

  private controller() { const controller = new AbortController(); this.controllers.add(controller); return controller; }
  private release(controller: AbortController) { this.controllers.delete(controller); }
  private cancelPending() { for (const controller of this.controllers) controller.abort(); this.controllers.clear(); }
}

export const session = new SessionStore();
