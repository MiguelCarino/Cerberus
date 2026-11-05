// models.js: pure helpers for the Models view. No DOM.

// deleteModelMessage words the confirm for deleting a local model from disk.
export function deleteModelMessage(m) {
  const size = m && m.SizeMiB ? (m.SizeMiB >= 1024 ? `${(m.SizeMiB / 1024).toFixed(1)} GB` : `${m.SizeMiB} MB`) : '';
  const how = {
    llamacpp: 'Its .gguf file is removed from your models folder.',
    lmstudio: "Its files are removed from LM Studio's models folder.",
    ollama: 'Ollama removes it.',
  }[m && m.Backend] || 'Its files are removed.';
  const parts = [how];
  if (size) parts.push(`That frees about ${size}.`);
  if (m && m.Loaded) parts.push('It is unloaded first.');
  parts.push('To use it again you have to download it again.');
  return { title: `Delete ${(m && m.ID) || 'model'}`, message: parts.join(' ') };
}

// busyText is a model's load line (gateway phase G5): how long the oldest
// request has been answering, the queue and its oldest wait, and stalls.
export function busyText(m) {
  const secs = (x) => (x >= 90 ? `${Math.round(x / 60)} min` : `${Math.round(x)} s`);
  const out = [];
  if (m.BusyForS >= 60) out.push(`busy for ${secs(m.BusyForS)}`);
  if (m.Waiting > 0) out.push(`${m.Waiting} queued${m.LongestWaitS >= 1 ? `, oldest ${secs(m.LongestWaitS)}` : ''}`);
  if (m.Stalls > 0) out.push(`${m.Stalls} stall${m.Stalls === 1 ? '' : 's'} since start`);
  return out.join(' · ');
}
