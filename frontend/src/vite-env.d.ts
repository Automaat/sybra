/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_MODE?: string
  readonly VITE_API_BASE?: string
  readonly VITE_API_TOKEN?: string
}

interface Window {
  runtime?: unknown
}
