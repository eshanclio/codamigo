interface ButtonProps {
    label: string;
    onClick: () => void;
}

function Button({ label, onClick }: ButtonProps): JSX.Element {
    return <button onClick={onClick}>{label}</button>;
}

class Counter {
    private count: number = 0;

    increment(): void {
        this.count++;
    }

    getCount(): number {
        return this.count;
    }
}

export function App(): JSX.Element {
    return <div><Button label="Click me" onClick={() => {}} /></div>;
}
