// navigator.clipboard requires a secure context (https, or http on
// localhost/127.0.0.1). Self-hosted Observe is commonly reached over plain
// http via a Tailscale IP or LAN hostname, where navigator.clipboard is
// undefined — calling it directly throws. Fall back to the deprecated but
// still-functional execCommand path in that case.
export async function copyToClipboard(text: string): Promise<boolean> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // fall through to execCommand fallback
    }
  }
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  document.body.appendChild(textarea);
  textarea.focus();
  textarea.select();
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  }
  document.body.removeChild(textarea);
  return ok;
}
