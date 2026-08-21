import * as React from "react"

import { cn } from "@/shared/lib/cn"

type TableProps = React.HTMLAttributes<HTMLTableElement> & {
  viewportRows?: number
  rowHeight?: number
}

// TableScrollViewport sizes the inner scroll viewport from rendered metrics
// instead of a fixed 36px header assumption, and drops the maxHeight cap
// entirely when the table essentially fits the design viewport. Data rows
// carry a 1px bottom border that table layout renders on top of the fixed
// row height (h-[96px] → 97px), so a 20-row page overflowed the old constant
// by ~20px — a micro scrollbar that hijacked the wheel at the end of page
// scrolling. When natural height is within half a row of the cap the table is
// treated as "fits": the container never scrolls and the page takes over.
// Larger pages (100+ rows) exceed the cap by many rows and keep internal
// scrolling as designed.
function TableScrollViewport({ viewportRows, rowHeight, children }: {
  viewportRows: number
  rowHeight: number
  children: React.ReactNode
}) {
  const containerRef = React.useRef<HTMLDivElement>(null)
  const [measured, setMeasured] = React.useState<{ header: number; total: number } | null>(null)

  React.useLayoutEffect(() => {
    const container = containerRef.current
    if (!container || typeof ResizeObserver === "undefined") return
    const measure = () => {
      const table = container.querySelector("table")
      if (!table || !table.tHead) return
      const header = Math.ceil(table.tHead.getBoundingClientRect().height)
      const total = Math.ceil(table.getBoundingClientRect().height)
      setMeasured((current) =>
        current && Math.abs(current.header - header) <= 0.5 && Math.abs(current.total - total) <= 0.5
          ? current
          : { header, total }
      )
    }
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(container)
    return () => observer.disconnect()
  }, [])

  const header = measured?.header ?? 36
  const cap = header + viewportRows * rowHeight
  // Half a row of tolerance: content that merely overflows by row borders or
  // header rounding is "fits"; real multi-row overflow keeps the viewport.
  const fits = measured ? measured.total <= cap + rowHeight / 2 : false

  return (
    <div
      ref={containerRef}
      data-slot="table-scroll-container"
      className="relative w-full overflow-auto"
      style={fits ? undefined : { maxHeight: cap }}
    >
      {children}
    </div>
  )
}

const Table = React.forwardRef<HTMLTableElement, TableProps>(({ className, viewportRows, rowHeight, children, ...props }, ref) => {
  const stickyHeader = viewportRows !== undefined
  const table = (
    <table
      ref={ref}
      className={cn("w-full caption-bottom text-sm", stickyHeader && "[&_thead]:sticky [&_thead]:top-0 [&_thead]:z-30 [&_thead]:bg-background", className)}
      {...props}
    >
      {children}
    </table>
  )
  if (!stickyHeader || !rowHeight) {
    return (
      <div data-slot="table-scroll-container" className="relative w-full overflow-auto">
        {table}
      </div>
    )
  }
  return <TableScrollViewport viewportRows={viewportRows} rowHeight={rowHeight}>{table}</TableScrollViewport>
})
Table.displayName = "Table"

const TableHeader = React.forwardRef<
  HTMLTableSectionElement,
  React.HTMLAttributes<HTMLTableSectionElement>
>(({ className, ...props }, ref) => (
  <thead ref={ref} className={cn("[&_tr]:border-b", className)} {...props} />
))
TableHeader.displayName = "TableHeader"

const TableBody = React.forwardRef<
  HTMLTableSectionElement,
  React.HTMLAttributes<HTMLTableSectionElement>
>(({ className, ...props }, ref) => (
  <tbody
    ref={ref}
    className={cn("[&_tr:last-child]:border-0", className)}
    {...props}
  />
))
TableBody.displayName = "TableBody"

const TableFooter = React.forwardRef<
  HTMLTableSectionElement,
  React.HTMLAttributes<HTMLTableSectionElement>
>(({ className, ...props }, ref) => (
  <tfoot
    ref={ref}
    className={cn(
      "border-t bg-muted/50 font-medium [&>tr]:last:border-b-0",
      className
    )}
    {...props}
  />
))
TableFooter.displayName = "TableFooter"

const TableRow = React.forwardRef<
  HTMLTableRowElement,
  React.HTMLAttributes<HTMLTableRowElement>
>(({ className, ...props }, ref) => (
  <tr
    ref={ref}
    className={cn(
      "border-b transition-colors hover:bg-[color-mix(in_oklab,var(--secondary)_45%,var(--background))] data-[state=selected]:bg-secondary",
      className
    )}
    {...props}
  />
))
TableRow.displayName = "TableRow"

const TableHead = React.forwardRef<
  HTMLTableCellElement,
  React.ThHTMLAttributes<HTMLTableCellElement>
>(({ className, ...props }, ref) => (
  <th
    ref={ref}
    className={cn(
      "h-9 px-2 text-left align-middle text-xs font-normal text-muted-foreground [&:has([role=checkbox])]:pr-0 [&>[role=checkbox]]:translate-y-[1px]",
      className
    )}
    {...props}
  />
))
TableHead.displayName = "TableHead"

const TableCell = React.forwardRef<
  HTMLTableCellElement,
  React.TdHTMLAttributes<HTMLTableCellElement>
>(({ className, ...props }, ref) => (
  <td
    ref={ref}
    className={cn(
      "px-2 py-2 align-middle [&:has([role=checkbox])]:pr-0 [&>[role=checkbox]]:translate-y-[1px]",
      className
    )}
    {...props}
  />
))
TableCell.displayName = "TableCell"

const TableActionHead = React.forwardRef<
  HTMLTableCellElement,
  React.ThHTMLAttributes<HTMLTableCellElement>
>(({ className, ...props }, ref) => (
  <TableHead
    ref={ref}
    className={cn("sticky right-0 z-20 w-12 min-w-12 bg-background px-2", className)}
    {...props}
  />
))
TableActionHead.displayName = "TableActionHead"

const TableActionCell = React.forwardRef<
  HTMLTableCellElement,
  React.TdHTMLAttributes<HTMLTableCellElement>
>(({ className, ...props }, ref) => (
  <TableCell
    ref={ref}
    className={cn(
      "sticky right-0 z-10 w-12 min-w-12 bg-background px-2 transition-colors group-hover:bg-[color-mix(in_oklab,var(--secondary)_45%,var(--background))] group-data-[state=selected]:bg-secondary",
      className
    )}
    {...props}
  />
))
TableActionCell.displayName = "TableActionCell"

const TableCaption = React.forwardRef<
  HTMLTableCaptionElement,
  React.HTMLAttributes<HTMLTableCaptionElement>
>(({ className, ...props }, ref) => (
  <caption
    ref={ref}
    className={cn("mt-4 text-sm text-muted-foreground", className)}
    {...props}
  />
))
TableCaption.displayName = "TableCaption"

export {
  Table,
  TableHeader,
  TableBody,
  TableFooter,
  TableHead,
  TableRow,
  TableCell,
  TableActionHead,
  TableActionCell,
  TableCaption,
}
