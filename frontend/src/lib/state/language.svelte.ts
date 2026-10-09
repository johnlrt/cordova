export type UiLanguage = 'en' | 'ru';

class LanguageStore {
  value = $state<UiLanguage>('en');
  private loaded = false;

  load() {
    if (this.loaded || typeof window === 'undefined') return;
    this.loaded = true;
    try {
      const saved = localStorage.getItem('cordova-language');
      if (saved === 'en' || saved === 'ru') this.value = saved;
    } catch { /* Use English when browser storage is unavailable. */ }
    document.documentElement.lang = this.value;
  }

  set(value: UiLanguage) {
    this.value = value;
    if (typeof document !== 'undefined') document.documentElement.lang = value;
    try { localStorage.setItem('cordova-language', value); } catch { /* Language preference is optional. */ }
  }
}

export const language = new LanguageStore();
