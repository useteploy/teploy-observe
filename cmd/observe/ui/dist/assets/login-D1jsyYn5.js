import{u as o}from"./jsxRuntime-By7dKY42.js";import{h as r,y as p}from"./hooks-EqOrdhkf.js";import{a as l}from"./auth-CFVI_Mbd.js";import{k as f}from"./index-B37jIjyb.js";function v(){const[i,s]=r(l.isAuthenticated());return p(()=>{if(i||typeof window>"u")return;const n=window.location.pathname;n==="/login"||n==="/setup"||l.checkSetup().then(({needs_setup:e})=>{window.location.href=e?"/setup":"/login"}).catch(()=>{window.location.href="/login"})},[i]),{authenticated:i,login:async(n,e)=>{const a=await l.login(n,e);localStorage.setItem("obs_token",a.token),s(!0),window.location.href="/"},logout:()=>{l.logout(),s(!1)}}}const S={mode:"app"};function z(){const{login:i}=v(),[s,b]=r(""),[d,n]=r(""),[e,a]=r(""),[c,g]=r(!1),[u,h]=r(null);return p(()=>{if(typeof window<"u"){const t=new URLSearchParams(window.location.search).get("error");t&&a(t)}fetch("/api/v1/auth/methods").then(t=>t.ok?t.json():null).then(t=>{t&&t.oidc&&h({label:t.oidc_label||"Single sign-on"})}).catch(()=>{})},[]),o("div",{class:"obs-login-page",children:[o("form",{class:"obs-login-form",onSubmit:async t=>{t.preventDefault(),a(""),g(!0);try{await i(s,d)}catch{a("Invalid credentials"),g(!1)}},children:[o("h1",{class:"obs-login-title",children:"Teploy Observe"}),o("p",{class:"obs-login-subtitle",children:"Sign in to your dashboard"}),e&&o("div",{class:"obs-login-error",children:e}),u&&o(f,{children:[o("a",{class:"obs-sso-button",href:"/api/v1/auth/oidc/login",children:u.label}),o("div",{class:"obs-login-divider",children:"or"})]}),o("label",{class:"obs-login-label",children:["Username",o("input",{type:"text",class:"obs-login-input",value:s,onInput:t=>b(t.target.value),autoFocus:!0,required:!0})]}),o("label",{class:"obs-login-label",children:["Password",o("input",{type:"password",class:"obs-login-input",value:d,onInput:t=>n(t.target.value),required:!0})]}),o("button",{type:"submit",class:"obs-login-button",disabled:c,children:c?o("span",{style:{display:"flex",alignItems:"center",justifyContent:"center",gap:"8px"},children:[o("svg",{width:"16",height:"16",viewBox:"0 0 24 24",fill:"currentColor",style:{animation:"spin 1s linear infinite"},children:o("path",{d:"M12 4V2A10 10 0 0 0 2 12h2a8 8 0 0 1 8-8z"})}),"Signing in..."]}):"Sign in"})]}),o("style",{children:`
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
        .obs-sso-button {
          display: block; width: 100%; padding: 10px; margin-bottom: 8px;
          background: transparent; color: var(--obs-text);
          border: 1px solid var(--obs-border); border-radius: var(--obs-radius);
          font-size: 14px; font-weight: 600; font-family: var(--obs-font);
          text-align: center; text-decoration: none; cursor: pointer;
          transition: border-color var(--obs-transition);
        }
        .obs-sso-button:hover { border-color: var(--obs-accent); }
        .obs-login-divider {
          display: flex; align-items: center; gap: 10px;
          margin: 4px 0 16px; color: var(--obs-text-muted); font-size: 12px;
        }
        .obs-login-divider::before, .obs-login-divider::after {
          content: ""; flex: 1; height: 1px; background: var(--obs-border-subtle);
        }
        .obs-login-error {
          background: var(--obs-danger-bg); color: var(--obs-danger);
          padding: 8px 12px; border-radius: var(--obs-radius);
          font-size: 13px; margin-bottom: 16px; text-align: center;
        }
      `})]})}export{S as config,z as default};
