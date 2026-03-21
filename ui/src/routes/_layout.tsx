export default function Layout({ children }: { children: preact.ComponentChildren }) {
  return (
    <div style="font-family:'Inter',-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;background:#09090b;color:#fafafa;min-height:100vh;margin:0;">
      <div class="obs-accent-line" />
      <div style="max-width:1280px;margin:0 auto;padding:24px 24px 48px;">
        {children}
      </div>
    </div>
  );
}
