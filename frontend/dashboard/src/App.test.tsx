import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import App from "./App.tsx";

describe("App", () => {
  it("renders the dashboard heading", () => {
    render(<App />);
    expect(
      screen.getByRole("heading", { name: "PayGate Dashboard" }),
    ).toBeInTheDocument();
  });
});
