const localOrigin = 'https://cordova.invalid';

export function safeReturnPath(input: string | null | undefined, fallback = '/dashboard'): string {
  if (!input || !input.startsWith('/') || input.startsWith('//') || input.includes('\\') || /[\u0000-\u001f]/.test(input)) return fallback;
  try {
    const url = new URL(input, localOrigin);
    if (url.origin !== localOrigin || !url.pathname.startsWith('/') || url.pathname.startsWith('//')) return fallback;
    return `${url.pathname}${url.search}${url.hash}`;
  } catch {
    return fallback;
  }
}
