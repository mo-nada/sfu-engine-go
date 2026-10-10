export const metadata = { title: "SFU Engine – Next.js example" };

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body style={{ margin: 0, fontFamily: "system-ui, sans-serif", background: "#0b1020", color: "#e5e7eb" }}>
        {children}
      </body>
    </html>
  );
}
