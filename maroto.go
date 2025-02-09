package maroto

import (
	"errors"

	"github.com/johnfercher/maroto/v2/internal/cache"

	"github.com/johnfercher/maroto/v2/internal/providers/gofpdf"

	"github.com/johnfercher/maroto/v2/pkg/merge"

	"github.com/johnfercher/maroto/v2/pkg/core/entity"

	"github.com/johnfercher/go-tree/node"

	"github.com/johnfercher/maroto/v2/pkg/props"

	"github.com/f-amaral/go-async/async"
	"github.com/f-amaral/go-async/pool"
	"github.com/johnfercher/maroto/v2/pkg/components/col"
	"github.com/johnfercher/maroto/v2/pkg/components/page"
	"github.com/johnfercher/maroto/v2/pkg/components/row"
	"github.com/johnfercher/maroto/v2/pkg/config"
	"github.com/johnfercher/maroto/v2/pkg/core"
)

type maroto struct {
	config   *entity.Config
	provider core.Provider
	cache    cache.Cache

	// Building
	cell          entity.Cell
	pages         []core.Page
	rows          []core.Row
	header        []core.Row
	footer        []core.Row
	headerHeight  float64
	footerHeight  float64
	currentHeight float64

	// Processing
	pool async.Processor[[]core.Page, []byte]
}

func New(cfgs ...*entity.Config) core.Maroto {
	cache := cache.New()
	cfg := getConfig(cfgs...)
	provider := getProvider(cache, cfg)

	m := &maroto{
		provider: provider,
		cell: entity.NewRootContext(cfg.Dimensions.Width, cfg.Dimensions.Height, entity.Margins{
			Left:   cfg.Margins.Left,
			Top:    cfg.Margins.Top,
			Right:  cfg.Margins.Right,
			Bottom: cfg.Margins.Bottom,
		}),
		cache:  cache,
		config: cfg,
	}

	if cfg.WorkersQuantity > 0 {
		p := pool.NewPool[[]core.Page, []byte](cfg.WorkersQuantity, m.processPage,
			pool.WithSortingOutput[[]core.Page, []byte]())
		m.pool = p
	}
	return m
}

func (m *maroto) AddPages(pages ...core.Page) {
	for _, page := range pages {
		if m.currentHeight != m.headerHeight {
			m.fillPageToAddNew()
			m.addHeader()
		}
		m.addRows(page.GetRows()...)
	}
}

func (m *maroto) AddRows(rows ...core.Row) {
	m.addRows(rows...)
}

func (m *maroto) AddRow(rowHeight float64, cols ...core.Col) core.Row {
	r := row.New(rowHeight).Add(cols...)
	m.addRow(r)
	return r
}

func (m *maroto) RegisterHeader(rows ...core.Row) error {
	height := m.getRowsHeight(rows...)
	if height+m.footerHeight > m.config.Dimensions.Height {
		return errors.New("header height is greater than page useful area")
	}

	m.headerHeight = height
	m.header = rows

	for _, headerRow := range rows {
		m.addRow(headerRow)
	}

	return nil
}

func (m *maroto) RegisterFooter(rows ...core.Row) error {
	height := m.getRowsHeight(rows...)
	if height > m.config.Dimensions.Height {
		return errors.New("footer height is greater than page useful area")
	}

	m.footerHeight = height
	m.footer = rows
	return nil
}

func (m *maroto) Generate() (core.Document, error) {
	m.provider.SetProtection(m.config.Protection)
	m.provider.SetCompression(m.config.Compression)
	m.provider.SetMetadata(m.config.Metadata)

	m.fillPageToAddNew()
	m.setConfig()

	if m.config.WorkersQuantity > 0 {
		return m.generateConcurrently()
	}

	return m.generate()
}

func (m *maroto) GetStructure() *node.Node[core.Structure] {
	m.fillPageToAddNew()

	str := core.Structure{
		Type:    "maroto",
		Details: m.config.ToMap(),
	}
	node := node.New(str)

	for _, p := range m.pages {
		inner := p.GetStructure()
		node.AddNext(inner)
	}

	return node
}

func (m *maroto) addRows(rows ...core.Row) {
	for _, row := range rows {
		m.addRow(row)
	}
}

func (m *maroto) addRow(r core.Row) {
	maxHeight := m.cell.Height

	rowHeight := r.GetHeight()
	sumHeight := rowHeight + m.currentHeight + m.footerHeight

	// Row smaller than the remain space on page
	if sumHeight < maxHeight {
		m.currentHeight += rowHeight
		m.rows = append(m.rows, r)
		return
	}

	// As row will extrapolate page, we will add empty space
	// on the page to force a new page
	m.fillPageToAddNew()

	m.addHeader()

	// AddRows row on the new page
	m.currentHeight += rowHeight
	m.rows = append(m.rows, r)
}

func (m *maroto) addHeader() {
	for _, headerRow := range m.header {
		m.currentHeight += headerRow.GetHeight()
		m.rows = append(m.rows, headerRow)
	}
}

func (m *maroto) fillPageToAddNew() {
	space := m.cell.Height - m.currentHeight - m.footerHeight

	c := col.New(m.config.MaxGridSize)
	spaceRow := row.New(space)
	spaceRow.Add(c)

	m.rows = append(m.rows, spaceRow)
	m.rows = append(m.rows, m.footer...)

	prop := props.Page{
		Pattern: m.config.PageNumberPattern,
		Place:   m.config.PageNumberPlace,
		Family:  m.config.DefaultFont.Family,
		Style:   m.config.DefaultFont.Style,
		Size:    m.config.DefaultFont.Size,
		Color:   m.config.DefaultFont.Color,
	}
	p := page.New(prop)
	p.Add(m.rows...)

	m.pages = append(m.pages, p)
	m.rows = nil
	m.currentHeight = 0
}

func (m *maroto) setConfig() {
	for i, page := range m.pages {
		page.SetConfig(m.config)
		page.SetNumber(i+1, len(m.pages))
	}
}

func (m *maroto) generate() (core.Document, error) {
	innerCtx := m.cell.Copy()

	for _, page := range m.pages {
		page.Render(m.provider, innerCtx)
	}

	documentBytes, err := m.provider.GenerateBytes()
	if err != nil {
		return nil, err
	}

	return core.NewPDF(documentBytes, nil), nil
}

func (m *maroto) generateConcurrently() (core.Document, error) {
	chunks := len(m.pages) / m.config.WorkersQuantity
	if chunks == 0 {
		chunks = 1
	}
	pageGroups := make([][]core.Page, 0)
	for i := 0; i < len(m.pages); i += chunks {
		end := i + chunks

		if end > len(m.pages) {
			end = len(m.pages)
		}

		pageGroups = append(pageGroups, m.pages[i:end])
	}

	processed := m.pool.Process(pageGroups)
	if processed.HasError {
		return nil, errors.New("an error has occurred while trying to generate PDFs concurrently")
	}

	pdfs := make([][]byte, len(processed.Results))
	for i, result := range processed.Results {
		bytes := result.Output.([]byte)
		pdfs[i] = bytes
	}

	mergedBytes, err := merge.Bytes(pdfs...)
	if err != nil {
		return nil, err
	}

	return core.NewPDF(mergedBytes, nil), nil
}

func (m *maroto) processPage(pages []core.Page) ([]byte, error) {
	innerCtx := m.cell.Copy()

	innerProvider := getProvider(cache.NewMutexDecorator(cache.New()), m.config)
	for _, page := range pages {
		page.Render(innerProvider, innerCtx)
	}

	return innerProvider.GenerateBytes()
}

func (m *maroto) getRowsHeight(rows ...core.Row) float64 {
	var height float64
	for _, r := range rows {
		height += r.GetHeight()
	}

	return height
}

func getConfig(configs ...*entity.Config) *entity.Config {
	if len(configs) > 0 {
		return configs[0]
	}

	return config.NewBuilder().Build()
}

func getProvider(cache cache.Cache, cfg *entity.Config) core.Provider {
	deps := gofpdf.NewBuilder().Build(cfg, cache)
	return gofpdf.New(deps)
}
