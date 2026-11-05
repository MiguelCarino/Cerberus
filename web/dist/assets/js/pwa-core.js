// Pure helpers for the PWA glue (pwa.js). No DOM, so node --test imports
// them directly.

// updateReady: a new service worker is installed and waiting while an
// older one controls the page. On the very first install there is no
// controller, so nothing is offered.
export function updateReady(reg, controlled) {
  return !!(reg && reg.waiting && controlled);
}
