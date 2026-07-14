import{u as o}from"./jsxRuntime-BNKveKuz.js";import{h as a,y as u}from"./index-BZ0ygvrz.js";import{a as l}from"./auth-CFVI_Mbd.js";function p(){const[e,r]=a(l.isAuthenticated());return u(()=>{if(e||typeof window>"u")return;const t=window.location.pathname;t==="/login"||t==="/setup"||l.checkSetup().then(({needs_setup:n})=>{window.location.href=n?"/setup":"/login"}).catch(()=>{window.location.href="/login"})},[e]),{authenticated:e,login:async(t,n)=>{const s=await l.login(t,n);localStorage.setItem("obs_token",s.token),r(!0),window.location.href="/"},logout:()=>{l.logout(),r(!1)}}}const v={mode:"app"};function y(){const{login:e}=p(),[r,c]=a(""),[d,t]=a(""),[n,s]=a(""),[b,g]=a(!1);return o("div",{class:"obs-login-page",children:[o("form",{class:"obs-login-form",onSubmit:async i=>{i.preventDefault(),s(""),g(!0);try{await e(r,d)}catch{s("Invalid credentials"),g(!1)}},children:[o("div",{style:{display:"flex",justifyContent:"center",marginBottom:"16px"},children:o("div",{style:{width:"48px",height:"48px",borderRadius:"12px",background:"linear-gradient(135deg, var(--obs-accent), #a78bfa)",display:"flex",alignItems:"center",justifyContent:"center",fontSize:"22px",fontWeight:800,color:"#fff"},children:"O"})}),o("h1",{class:"obs-login-title",children:"Observe"}),o("p",{class:"obs-login-subtitle",children:"Sign in to your dashboard"}),n&&o("div",{class:"obs-login-error",children:n}),o("label",{class:"obs-login-label",children:["Username",o("input",{type:"text",class:"obs-login-input",value:r,onInput:i=>c(i.target.value),autoFocus:!0,required:!0})]}),o("label",{class:"obs-login-label",children:["Password",o("input",{type:"password",class:"obs-login-input",value:d,onInput:i=>t(i.target.value),required:!0})]}),o("button",{type:"submit",class:"obs-login-button",disabled:b,children:b?o("span",{style:{display:"flex",alignItems:"center",justifyContent:"center",gap:"8px"},children:[o("svg",{width:"16",height:"16",viewBox:"0 0 24 24",fill:"currentColor",style:{animation:"spin 1s linear infinite"},children:o("path",{d:"M12 4V2A10 10 0 0 0 2 12h2a8 8 0 0 1 8-8z"})}),"Signing in..."]}):"Sign in"})]}),o("style",{children:`
        @keyframes spin { to { transform: rotate(360deg); } }
        .obs-login-page {
          display: flex; align-items: center; justify-content: center;
          min-height: 100vh;
          background: var(--obs-bg);
          background-image: radial-gradient(ellipse at 50% 0%, rgba(99, 102, 241, 0.08) 0%, transparent 60%);
        }
        .obs-login-form {
          width: 100%; max-width: 360px; padding: 32px;
          background: var(--obs-surface); border: 1px solid var(--obs-border-subtle);
          border-radius: var(--obs-radius-lg);
        }
        .obs-login-title {
          font-size: 24px; font-weight: 700; margin: 0 0 4px;
          color: var(--obs-text); text-align: center;
        }
        .obs-login-subtitle {
          font-size: 13px; color: var(--obs-text-muted);
          margin: 0 0 24px; text-align: center;
        }
        .obs-login-label {
          display: block; font-size: 12px; font-weight: 500;
          color: var(--obs-text-secondary); margin-bottom: 16px;
        }
        .obs-login-input {
          display: block; width: 100%; margin-top: 6px; padding: 10px 12px;
          background: var(--obs-bg); border: 1px solid var(--obs-border);
          border-radius: var(--obs-radius); color: var(--obs-text);
          font-size: 14px; font-family: var(--obs-font);
          transition: border-color var(--obs-transition);
          outline: none;
        }
        .obs-login-input:focus { border-color: var(--obs-accent); }
        .obs-login-button {
          display: block; width: 100%; padding: 10px; margin-top: 8px;
          background: var(--obs-accent); color: white; border: none;
          border-radius: var(--obs-radius); font-size: 14px; font-weight: 600;
          font-family: var(--obs-font); cursor: pointer;
          transition: background var(--obs-transition);
        }
        .obs-login-button:hover { background: var(--obs-accent-hover); }
        .obs-login-button:disabled { opacity: 0.6; cursor: not-allowed; }
        .obs-login-error {
          background: var(--obs-danger-bg); color: var(--obs-danger);
          padding: 8px 12px; border-radius: var(--obs-radius);
          font-size: 13px; margin-bottom: 16px; text-align: center;
        }
      `})]})}export{v as config,y as default};
