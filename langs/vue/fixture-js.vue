<template>
  <div>
    <span>{{ message }}</span>
    <button @click="greet">Hello</button>
  </div>
</template>

<script>
export function greet(name) {
  return "Hello, " + name;
}

export function formatMessage(text) {
  return text.trim();
}
</script>

<style>
div { padding: 8px; }
</style>
