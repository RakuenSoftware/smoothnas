# SmoothNAS Frontend

This is the active frontend workspace for SmoothNAS.

The app is React + Vite only.

It is not a generic admin dashboard scaffold. It is the operator-facing control surface for:

- disks and SMART
- RAID arrays
- named tiers
- ZFS objects
- sharing services
- benchmarks
- networking
- updates and system controls

## Dev Commands

Install dependencies:

```bash
npm install
```

Start the development server:

```bash
npm run dev
```

Run tests:

```bash
npm test
```

Build production assets:

```bash
npm run build
```

## Frontend Structure

Important paths:

- `src/main.tsx` for the active browser bootstrap
- `src/App.tsx` for the active route structure
- `src/pages` for the active React feature pages
- `src/components` for the active React UI building blocks

The frontend assumes a backend that prefers asynchronous jobs for slow operations. Many workflows therefore follow this pattern:

1. start an operation
2. receive a `job_id`
3. poll job state
4. refresh the affected page data when the job completes

## Related Documentation

- [../README.md](../README.md) for the product-level guide
- [../src/README.md](../src/README.md) for the technical deep dive
- [../docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md) for system diagrams
- [../docs/AIMEE.md](../docs/AIMEE.md) for agent-consumer guidance
