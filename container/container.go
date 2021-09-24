package container

import (
	"bytes"
	"fmt"
	"reflect"

	"github.com/goccy/go-graphviz/cgraph"
	"github.com/pkg/errors"
)

type container struct {
	*config

	resolvers map[reflect.Type]resolver

	scopes map[string]Scope

	resolveStack []resolveFrame
	callerStack  []Location
	callerMap    map[Location]bool
}

type resolveFrame struct {
	loc Location
	typ reflect.Type
}

func newContainer(cfg *config) *container {
	return &container{
		config:      cfg,
		resolvers:   map[reflect.Type]resolver{},
		scopes:      map[string]Scope{},
		callerStack: nil,
		callerMap:   map[Location]bool{},
	}
}

func (c *container) call(constructor *ProviderDescriptor, scope Scope) ([]reflect.Value, error) {
	loc := constructor.Location
	graphNode, err := c.locationGraphNode(loc, scope)
	if err != nil {
		return nil, err
	}
	markGraphNodeAsFailed(graphNode)

	if c.callerMap[loc] {
		return nil, errors.Errorf("cyclic dependency: %s -> %s", loc.Name(), loc.Name())
	}

	c.callerMap[loc] = true
	c.callerStack = append(c.callerStack, loc)

	c.logf("Resolving dependencies for %s", loc)
	c.indentLogger()
	inVals := make([]reflect.Value, len(constructor.Inputs))
	for i, in := range constructor.Inputs {
		val, err := c.resolve(in, scope, loc)
		if err != nil {
			return nil, err
		}
		inVals[i] = val
	}
	c.dedentLogger()
	c.logf("Calling %s", loc)

	delete(c.callerMap, loc)
	c.callerStack = c.callerStack[0 : len(c.callerStack)-1]

	out, err := constructor.Fn(inVals)
	if err != nil {
		return nil, errors.Wrapf(err, "error calling constructor %s", loc)
	}

	markGraphNodeAsUsed(graphNode)

	return out, nil
}

func (c *container) addNode(constructor *ProviderDescriptor, scope Scope) (interface{}, error) {
	constructorGraphNode, err := c.locationGraphNode(constructor.Location, scope)
	if err != nil {
		return reflect.Value{}, err
	}

	hasScopeParam := false
	for _, in := range constructor.Inputs {
		if in.Type == scopeType {
			hasScopeParam = true
		}

		typeGraphNode, err := c.typeGraphNode(in.Type)
		if err != nil {
			return reflect.Value{}, err
		}

		c.addGraphEdge(typeGraphNode, constructorGraphNode)
	}

	if scope != nil || !hasScopeParam {
		c.logf("Registering %s", constructor.Location.String())
		c.indentLogger()
		defer c.dedentLogger()

		node := &simpleProvider{
			provider: constructor,
			scope:    scope,
		}

		constructorGraphNode, err := c.locationGraphNode(constructor.Location, scope)
		if err != nil {
			return reflect.Value{}, err
		}

		for i, out := range constructor.Outputs {
			typ := out.Type

			// one-per-scope maps can't be used as a return type
			if typ.Kind() == reflect.Map && isOnePerScopeType(typ.Elem()) && typ.Key().Kind() == reflect.String {
				return nil, fmt.Errorf("%v cannot be used as a return type because %v is a one-per-scope type",
					typ, typ.Elem())
			}

			// auto-group slices of auto-group types
			if typ.Kind() == reflect.Slice && isAutoGroupType(typ.Elem()) {
				typ = typ.Elem()
			}

			vr, ok := c.resolvers[typ]
			if ok {
				c.logf("Found resolver for %v: %T", typ, vr)
				err := vr.addNode(node, i, c)
				if err != nil {
					return nil, err
				}
			} else {
				var typeGraphNode *cgraph.Node
				var err error

				if isAutoGroupType(typ) {
					c.logf("Registering resolver for auto-group type %v", typ)
					sliceType := reflect.SliceOf(typ)

					typeGraphNode, err = c.typeGraphNode(sliceType)
					if err != nil {
						return reflect.Value{}, err
					}
					typeGraphNode.SetComment("auto-group")

					r := &groupResolver{
						typ:       typ,
						sliceType: sliceType,
					}

					err = r.addNode(node, i, c)
					if err != nil {
						return nil, err
					}

					c.resolvers[typ] = r
					c.resolvers[sliceType] = &sliceGroupResolver{r}

				} else if isOnePerScopeType(typ) {
					c.logf("Registering resolver for one-per-scope type %v", typ)
					mapType := reflect.MapOf(stringType, typ)

					typeGraphNode, err = c.typeGraphNode(mapType)
					if err != nil {
						return reflect.Value{}, err
					}
					typeGraphNode.SetComment("one-per-scope")

					r := &onePerScopeResolver{
						typ:       typ,
						mapType:   mapType,
						providers: map[Scope]*simpleProvider{},
						idxMap:    map[Scope]int{},
					}

					err = r.addNode(node, i, c)
					if err != nil {
						return nil, err
					}

					c.resolvers[typ] = r
					c.resolvers[mapType] = &mapOfOnePerScopeResolver{r}
				} else {
					c.logf("Registering resolver for simple type %v", typ)

					typeGraphNode, err = c.typeGraphNode(typ)
					if err != nil {
						return reflect.Value{}, err
					}

					c.resolvers[typ] = &simpleResolver{
						node: node,
						typ:  typ,
					}
				}

				c.addGraphEdge(constructorGraphNode, typeGraphNode)
			}
		}

		return node, nil
	} else {
		c.logf("Registering scope provider: %s", constructor.Location.String())
		c.indentLogger()
		defer c.dedentLogger()

		node := &scopeDepProvider{
			provider:       constructor,
			calledForScope: map[Scope]bool{},
			valueMap:       map[Scope][]reflect.Value{},
		}

		for i, out := range constructor.Outputs {
			typ := out.Type

			c.logf("Registering resolver for scoped type %v", typ)

			existing, ok := c.resolvers[typ]
			if ok {
				return nil, errors.Errorf("duplicate provision of type %v by scoped provider %s\n\talready provided by %s",
					typ, constructor.Location, existing.describeLocation())
			}
			c.resolvers[typ] = &scopeDepResolver{
				typ:         typ,
				idxInValues: i,
				node:        node,
				valueMap:    map[Scope]reflect.Value{},
			}

			typeGraphNode, err := c.typeGraphNode(typ)
			if err != nil {
				return reflect.Value{}, err
			}

			c.addGraphEdge(constructorGraphNode, typeGraphNode)
		}

		return node, nil
	}
}

func (c *container) supply(value reflect.Value, location Location) error {
	typ := value.Type()
	locGrapNode, err := c.locationGraphNode(location, nil)
	if err != nil {
		return err
	}
	markGraphNodeAsUsed(locGrapNode)

	typeGraphNode, err := c.typeGraphNode(typ)
	if err != nil {
		return err
	}

	c.addGraphEdge(locGrapNode, typeGraphNode)

	if existing, ok := c.resolvers[typ]; ok {
		return duplicateDefinitionError(typ, location, existing.describeLocation())
	}

	c.resolvers[typ] = &supplyResolver{
		typ:   typ,
		value: value,
		loc:   location,
	}

	return nil
}

func (c *container) resolve(in ProviderInput, scope Scope, caller Location) (reflect.Value, error) {
	c.resolveStack = append(c.resolveStack, resolveFrame{loc: caller, typ: in.Type})

	typeGraphNode, err := c.typeGraphNode(in.Type)
	if err != nil {
		return reflect.Value{}, err
	}

	if in.Type == scopeType {
		if scope == nil {
			return reflect.Value{}, errors.Errorf("trying to resolve %T for %s but not inside of any scope", scope, caller)
		}
		c.logf("Providing Scope %s", scope.Name())
		markGraphNodeAsUsed(typeGraphNode)
		return reflect.ValueOf(scope), nil
	}

	vr, ok := c.resolvers[in.Type]
	if !ok {
		if in.Optional {
			c.logf("Providing zero value for optional dependency %v", in.Type)
			return reflect.Zero(in.Type), nil
		}

		markGraphNodeAsFailed(typeGraphNode)
		return reflect.Value{}, errors.Errorf("can't resolve type %v for %s:\n%s",
			in.Type, caller, c.formatResolveStack())
	}

	res, err := vr.resolve(c, scope, caller)
	if err != nil {
		markGraphNodeAsFailed(typeGraphNode)
		return reflect.Value{}, err
	}

	markGraphNodeAsUsed(typeGraphNode)

	c.resolveStack = c.resolveStack[:len(c.resolveStack)-1]

	return res, nil
}

func (c *container) run(invoker interface{}) error {
	rctr, err := ExtractProviderDescriptor(invoker)
	if err != nil {
		return err
	}

	if len(rctr.Outputs) > 0 {
		return errors.Errorf("invoker function cannot have return values other than error: %s", rctr.Location)
	}

	c.logf("Registering invoker")
	c.indentLogger()

	node, err := c.addNode(&rctr, nil)
	if err != nil {
		return err
	}

	c.dedentLogger()

	sn, ok := node.(*simpleProvider)
	if !ok {
		return errors.Errorf("cannot run scoped provider as an invoker")
	}

	c.logf("Building container")
	_, err = sn.resolveValues(c)
	if err != nil {
		return err
	}
	c.logf("Done building container")

	return nil
}

func (c container) createOrGetScope(name string) Scope {
	if s, ok := c.scopes[name]; ok {
		return s
	}
	s := newScope(name)
	c.scopes[name] = s
	return s
}

func (c container) formatResolveStack() string {
	buf := &bytes.Buffer{}
	_, _ = fmt.Fprintf(buf, "\twhile resolving:\n")
	n := len(c.resolveStack)
	for i := n - 1; i >= 0; i-- {
		rk := c.resolveStack[i]
		_, _ = fmt.Fprintf(buf, "\t\t%v for %s\n", rk.typ, rk.loc)
	}
	return buf.String()
}

func markGraphNodeAsUsed(node *cgraph.Node) {
	node.SetColor("black")
}

func markGraphNodeAsFailed(node *cgraph.Node) {
	node.SetColor("red")
}
