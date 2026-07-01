import { useState } from "preact/hooks";
import { copyToClipboard } from "../../lib/clipboard.js";

interface Props {
  code: string;
  language?: string;
  maxHeight?: string;
}

export default function CodeBlock({ code, maxHeight = "400px" }: Props) {
  const [copied, setCopied] = useState(false);

  const handleCopy = async () => {
    if (await copyToClipboard(code)) {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  };

  return (
    <div class="obs-code-block-wrap" style={{ position: "relative" }}>
      <button class="obs-code-copy" onClick={handleCopy} aria-label="Copy code">
        {copied ? "Copied" : "Copy"}
      </button>
      <pre class="obs-code-block" style={{ maxHeight }}>
        <code>{code}</code>
      </pre>
    </div>
  );
}
