import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

// The browser sends credentials only to its own origin. This target is configured
// by the operator, never by a URL supplied by a webpage or API response.
const target = process.env.CORDOVA_API_TARGET || 'http://127.0.0.1:8080';
const proxy = { '/healthz': { target }, '/v1': { target } };

export default defineConfig({
  plugins: [sveltekit()],
  server: { port: 5173, strictPort: true, proxy },
  preview: { port: 4173, strictPort: true, proxy }
});
