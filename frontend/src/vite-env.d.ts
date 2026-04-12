/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_MODE?: string
  readonly VITE_API_BASE?: string
}

interface Window {
  runtime?: unknown
}
