<script lang="ts">
  import { Greet } from '../wailsjs/go/main/App.js'

  let resultText = $state('Please enter your name below')
  let name = $state('')

  async function greet() {
    resultText = await Greet(name)
  }
</script>

<div class="h-full flex flex-col items-center justify-center gap-4 p-8">
  <h1 class="h1">Synapse</h1>
  <p class="text-lg">{resultText}</p>
  <div class="flex gap-2">
    <input
      class="input"
      type="text"
      placeholder="Enter your name"
      bind:value={name}
    />
    <button class="btn preset-filled" onclick={greet}>Greet</button>
  </div>
</div>
