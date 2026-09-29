# Production Dockerfile for Bun web application
FROM oven/bun:latest
WORKDIR /app

# Copy application files (including pre-installed node_modules if present)
COPY package.json bun.lock* ./
COPY . .

# Install dependencies if not already present, then build frontend bundle
ENV NODE_ENV=production
RUN if [ ! -d "node_modules" ] || [ ! -d "node_modules/react" ]; then bun install --frozen-lockfile; fi
RUN bun run build

EXPOSE 3000
ENV PORT=3000

# Run as non-root user
USER bun

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD bun -e 'fetch("http://localhost:" + (process.env.PORT || 3000) + "/api/hello").then(r => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))'

CMD ["bun", "run", "start"]
