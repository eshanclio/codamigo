class Greeter {
    constructor(name) {
        this.name = name;
    }

    greet() {
        return `Hello, ${this.name}`;
    }
}

function formatName(first, last) {
    return `${first} ${last}`;
}
