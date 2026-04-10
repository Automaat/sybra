import './style.css'
import { mount } from 'svelte'
import App from './App.svelte'

type ColorScheme = 'system' | 'light' | 'dark'

function applyColorScheme(scheme: ColorScheme) {
  const prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches
  const isDark = scheme === 'dark' || (scheme === 'system' && prefersDark)
  document.documentElement.classList.toggle('dark', isDark)
}

const saved = (localStorage.getItem('colorScheme') ?? 'system') as ColorScheme
applyColorScheme(saved)

window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
  const current = (localStorage.getItem('colorScheme') ?? 'system') as ColorScheme
  if (current === 'system') applyColorScheme('system')
})

const app = mount(App, {
  target: document.getElementById('app')!,
})

export default app
