<template>
  <div class="user-card">
    <h2>{{ fullName }}</h2>
    <p>{{ email }}</p>
    <button @click="submitForm">Save</button>
  </div>
</template>

<script lang="ts">
export function validateEmail(email: string): boolean {
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email);
}

export function formatFullName(first: string, last: string): string {
  return `${first} ${last}`.trim();
}

export function submitForm(data: Record<string, string>): void {
  if (!validateEmail(data.email)) {
    throw new Error("invalid email");
  }
  console.log("submitting", data);
}
</script>

<style>
.user-card {
  padding: 1rem;
  border: 1px solid #ccc;
}
</style>
