class UserService {
    private users: string[] = [];

    addUser(name: string): void {
        this.users.push(name);
    }

    getUsers(): string[] {
        return this.users;
    }
}

function getUserById(id: number): string {
    return `user-${id}`;
}
