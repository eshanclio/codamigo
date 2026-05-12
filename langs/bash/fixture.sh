#!/bin/bash

greet() {
    local name="$1"
    echo "Hello, $name"
}

setup() {
    echo "Setting up..."
}

greet "World"
