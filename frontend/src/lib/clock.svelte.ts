let nowMs = $state(Date.now())

const timer = setInterval(() => {
  nowMs = Date.now()
}, 1000)

if (import.meta.hot) {
  import.meta.hot.dispose(() => clearInterval(timer))
}

export const clock = {
  get now() {
    return nowMs
  },
}
