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
