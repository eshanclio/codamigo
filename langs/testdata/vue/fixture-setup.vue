<template>
  <div class="counter">
    <p>{{ count }}</p>
    <button @click="increment">+</button>
  </div>
</template>

<script setup lang="ts">
import { ref } from "vue";

const count = ref(0);

function increment(): void {
  count.value++;
}

function reset(): void {
  count.value = 0;
}
</script>

<style scoped>
.counter {
  display: flex;
  gap: 0.5rem;
}
</style>
